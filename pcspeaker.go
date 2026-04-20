// Package gobeep86 simulates IBM PC speaker playback from PIT divisor
// sequences or pre-encoded PCM that is re-driven through a 1-bit speaker path.
package gobeep86

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
)

// Default timing and hardware constants used by the package.
const (
	// DefaultTickRate is the default tick rate used for one-shot tone sequences.
	DefaultTickRate         = 140
	// DefaultMixTickRate is the preferred tick rate for emulated mixed effect playback.
	DefaultMixTickRate      = 280
	// DefaultPCMUpdateRate is the control/update rate used for PCM-driven speaker input.
	DefaultPCMUpdateRate    = 11025
	// DefaultOutputSampleRate is the default rendered PCM sample rate.
	DefaultOutputSampleRate = 44100
	// PITHz is the PIT channel clock frequency used to derive output tones.
	PITHz                   = 1193181
)

const pcmCompactThresholdBytes = 64 * 1024
const toneInterleaveMinCycles = 1.0

var toneInterleaveTargetHz = 140.0

// SetInterleaveHz sets the target tone-switch rate used when effect and music
// tones must share a single simulated speaker.
//
// Values are clamped to the range 10..1000 Hz.
func SetInterleaveHz(hz float64) {
	if hz < 10 {
		hz = 10
	} else if hz > 1000 {
		hz = 1000
	}
	toneInterleaveTargetHz = hz
}

// Tone represents one PC speaker tick.
//
// When Active is false the divisor is ignored and the speaker is silent.
type Tone struct {
	Active  bool
	Divisor uint16
}

// ToneDivisor returns the effective PIT divisor for the tone, or 0 when the
// tone is inactive.
func (t Tone) ToneDivisor() uint16 {
	if !t.Active {
		return 0
	}
	return t.Divisor
}

// ToneFrequency returns the tone frequency in Hz, or 0 for silence.
func (t Tone) ToneFrequency() float64 {
	if !t.Active || t.Divisor == 0 {
		return 0
	}
	return float64(PITHz) / float64(t.Divisor)
}

// PITDivisorForFrequency converts a frequency in Hz to the nearest PIT divisor.
//
// Non-positive frequencies return 0. Results are clamped to the 16-bit PIT range.
func PITDivisorForFrequency(freq float64) uint16 {
	if !(freq > 0) {
		return 0
	}
	divisor := int(math.Round(float64(PITHz) / freq))
	if divisor < 1 {
		divisor = 1
	}
	if divisor > math.MaxUint16 {
		divisor = math.MaxUint16
	}
	return uint16(divisor)
}

// Variant selects the speaker model used when rendering output.
type Variant int

const (
	// VariantClean outputs a mostly direct square-wave signal with minimal coloration.
	VariantClean Variant = iota
	// VariantSmallSpeaker models a small paper-cone PC speaker.
	VariantSmallSpeaker
	// VariantPiezo models a brighter buzzer / piezo-style speaker.
	VariantPiezo
)

// String returns a stable human-readable name for the variant.
func (v Variant) String() string {
	switch v {
	case VariantClean:
		return "passthrough"
	case VariantPiezo:
		return "small-buzzer"
	default:
		return "paper-speaker"
	}
}

// ParseVariant parses a speaker variant name and returns VariantSmallSpeaker
// for unknown values.
func ParseVariant(s string) Variant {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "passthrough", "clean", "dry", "none":
		return VariantClean
	case "small-buzzer", "piezo", "pizeo", "moving-iron", "movingiron", "tiny":
		return VariantPiezo
	default:
		return VariantSmallSpeaker
	}
}

type caseReverb struct {
	comb    [4][128]float64
	combPos [4]int
	combLen [4]int
	combFB  [4]float64
	ap      [2][64]float64
	apPos   [2]int
	apLen   [2]int
	apG     [2]float64
}

func newCaseReverb() caseReverb {
	r := caseReverb{}
	r.combLen = [4]int{102, 90, 72, 59}
	r.combFB = [4]float64{0.86, 0.84, 0.82, 0.80}
	r.apLen = [2]int{47, 23}
	r.apG = [2]float64{0.7, 0.7}
	return r
}

func (r *caseReverb) process(in float64) float64 {
	out := 0.0
	for i := 0; i < 4; i++ {
		delayed := r.comb[i][r.combPos[i]]
		r.comb[i][r.combPos[i]] = in + delayed*r.combFB[i]
		r.combPos[i]++
		if r.combPos[i] >= r.combLen[i] {
			r.combPos[i] = 0
		}
		out += delayed
	}
	out *= 0.25
	for i := 0; i < 2; i++ {
		delayed := r.ap[i][r.apPos[i]]
		v := out - r.apG[i]*delayed
		r.ap[i][r.apPos[i]] = v
		r.apPos[i]++
		if r.apPos[i] >= r.apLen[i] {
			r.apPos[i] = 0
		}
		out = delayed + r.apG[i]*v
	}
	return out
}

func (r *caseReverb) reset() { *r = newCaseReverb() }

// Source renders PC speaker output as stereo 16-bit little-endian PCM.
//
// A Source can play effect tones, music tones, or PCM-driven music that is
// re-encoded through the speaker path.
type Source struct {
	mu      sync.Mutex
	rate    int
	variant Variant
	model   speakerModel

	effectSeq         []Tone
	effectTickRate    int
	effectSamplePos   int
	effectMixPhase    float64
	effectMixDivisor  uint16
	effectMixActive   bool
	musicSeq          []Tone
	musicTickRate     int
	musicSamplePos    int
	musicLoop         bool
	musicPCM          []byte
	musicPCMPos       int
	musicPCMRate      int
	musicPCMPhase     float64
	musicPCMTarget    float64
	musicPCMTargetOK  bool
	musicPCMError     float64
	musicPCMActive    bool
	musicPCMClosed    bool
	musicPCMLoop      bool
	musicPCMEnv       float64
	musicPCMAGain     float64
	musicPCMHPPrevIn  float64
	musicPCMHPPrevOut float64
	musicPCMLPState   float64
	streamGain        float64
	toneMixSample     uint64
	pitPhase          float64
	lastDivisor       uint16
	lastActive        bool
	lastDirect        bool
	vel               float64
	disp              float64
	hp1Prev           float64
	hp1Out            float64
	hp2Prev           float64
	hp2Out            float64
	reverb            caseReverb
	dcPrevIn          float64
	dcPrevOut         float64
	lpState           float64
	envState          float64
	driveState        float64
	resY1             float64
	resY2             float64
	res2Y1            float64
	res2Y2            float64
	acY1              float64
	acY2              float64
	hpState           float64
	driftPhase        float64
	piezoPhase        float64
	freqSmooth        float64
	lastFreq          float64
	lastPiezoIn       float64
	piezoTickAge      int
	noiseState        uint32
}

type speakerModel struct {
	hpAlpha       float64
	gain          float64
	reverbMix     float64
	restLength    float64
	restStiffness float64
	restDamping   float64
	coneRadius    float64
}

const (
	spkConeDiameterMM = 40.0
	spkRadiusMetres   = spkConeDiameterMM / 1000.0 / 2
	spkSpeedOfSound   = 343.0
	spkAcousticCutoff = spkSpeedOfSound / (2 * math.Pi * spkRadiusMetres)
	spkK              = 0.01300
	spkD              = 0.02850
	spkBl             = 0.5
	spkVdrive         = 4.0
	spkRtotal         = 41.0
	spkMass           = 0.002
	spkDrive          = spkBl * spkVdrive / (spkRtotal * spkMass * DefaultOutputSampleRate * DefaultOutputSampleRate)
	spkGain           = 0.00176 * 4_000_000.0 / spkDrive * 2
	spkReverbMix      = 1.0
)

var spkHPAlpha = 1.0 / (1.0 + 2*math.Pi*spkAcousticCutoff/DefaultOutputSampleRate)
var cleanDCBlockAlpha = 1.0 / (1.0 + 2*math.Pi*20.0/DefaultOutputSampleRate)

const (
	piezoReOhms        = 4.5
	piezoLeHenries     = 0.0053
	piezoF0Hz          = 2400.0
	piezoQ             = 7.0
	piezoCabF0Hz       = 4100.0
	piezoCabQ          = 4.8
	piezoHoleF0Hz      = 2850.0
	piezoHoleQ         = 6.8
	piezoRadiationHPHz = 2400.0
	piezoDriveGain     = 44.0
	piezoPrimaryMix    = 0.18
	piezoSecondaryMix  = 0.08
	piezoHoleMix       = 3.8
	piezoDriveAsymPos  = 1.15
	piezoDriveAsymNeg  = 0.90
	piezoDriftHz       = 0.55
	piezoDriftRangeHz  = 10.0
	piezoNoiseMix      = 0.0003
	agcTarget          = 0.995
	agcMinGain         = 1.0
	agcMaxGain         = 192.0
	pcmHighPassHz      = 180.0
	pcmLowPassHz       = 3200.0
	agcAttackMS        = 0.75
	agcReleaseMS       = 10.0
	agcGainRiseMS      = 0.50
	agcGainFallMS      = 6.0
)

var piezoElectricalAlpha = math.Exp(-2 * math.Pi * (piezoReOhms / (2 * math.Pi * piezoLeHenries)) / DefaultOutputSampleRate)
var piezoHPAlpha = math.Exp(-2 * math.Pi * piezoRadiationHPHz / DefaultOutputSampleRate)

// NewSource creates a playback source using the requested speaker variant.
//
// New sources default to DefaultOutputSampleRate.
func NewSource(variant Variant) *Source {
	s := &Source{rate: DefaultOutputSampleRate, reverb: newCaseReverb(), streamGain: 1}
	s.SetVariant(variant)
	return s
}

func modelForVariant(v Variant) speakerModel {
	switch v {
	case VariantClean:
		return speakerModel{hpAlpha: 1.0, gain: math.MaxInt16 * 0.85 * 0.5011872336272722}
	case VariantPiezo:
		return speakerModel{hpAlpha: 1.0, gain: 1.0, reverbMix: spkReverbMix}
	default:
		return speakerModel{hpAlpha: spkHPAlpha, gain: spkGain, reverbMix: spkReverbMix, restLength: 0.01300, restStiffness: 0.02850, coneRadius: spkRadiusMetres}
	}
}

// SetVariant switches the speaker model used for future output.
func (s *Source) SetVariant(v Variant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.variant = v
	s.model = modelForVariant(v)
}

// SetGain applies an input gain before speaker modeling.
func (s *Source) SetGain(v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamGain = clampVolume(v)
}

// TotalSamples reports the total number of output frames currently available.
func (s *Source) TotalSamples() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalSamplesLocked()
}

func (s *Source) totalSamplesLocked() int {
	if s.rate <= 0 {
		return 0
	}
	return max(s.effectTotalSamplesLocked(), max(s.musicTotalSamplesLocked(), s.musicPCMTotalSamplesLocked()))
}

func (s *Source) effectTotalSamplesLocked() int {
	if s.rate <= 0 || len(s.effectSeq) == 0 {
		return 0
	}
	return totalSamplesForToneSeq(len(s.effectSeq), s.rate, s.effectTickRate)
}

func (s *Source) musicTotalSamplesLocked() int {
	if s.rate <= 0 || len(s.musicSeq) == 0 {
		return 0
	}
	return totalSamplesForToneSeq(len(s.musicSeq), s.rate, s.musicTickRate)
}

func (s *Source) musicPCMTotalSamplesLocked() int {
	if s.rate <= 0 || len(s.musicPCM) == 0 {
		return 0
	}
	return len(s.musicPCM) / 4
}

// Load replaces the current effect sequence and resets playback state.
//
// The sequence is played at DefaultTickRate.
func (s *Source) Load(seq []Tone, sampleRate int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.effectSeq = append([]Tone(nil), seq...)
	s.rate = sampleRate
	s.effectTickRate = DefaultTickRate
	s.effectSamplePos = 0
	s.resetStateLocked()
}

// SetEffectMixed loads an effect sequence intended to share the speaker with
// music playback through the package's emulated mixing path.
func (s *Source) SetEffectMixed(seq []Tone, sampleRate int, tickRate int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.effectSeq, s.effectTickRate = prepareEffectSeqForEmulatedMix(seq, tickRate)
	s.rate = sampleRate
	s.effectSamplePos = 0
	s.toneMixSample = 0
	s.effectMixPhase = 0
	s.effectMixDivisor = 0
	s.effectMixActive = false
}

// SetMusic loads a tone sequence as music playback.
//
// Calling SetMusic clears any PCM music state.
func (s *Source) SetMusic(seq []Tone, sampleRate int, tickRate int, loop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.musicSeq = append([]Tone(nil), seq...)
	s.rate = sampleRate
	s.musicTickRate = tickRate
	s.musicLoop = loop
	s.musicSamplePos = 0
	s.musicPCM = nil
	s.musicPCMPos = 0
	s.musicPCMRate = 0
	s.musicPCMPhase = 0
	s.musicPCMTarget = 0
	s.musicPCMTargetOK = false
	s.musicPCMError = 0
	s.musicPCMEnv = 0
	s.musicPCMAGain = 1
	s.musicPCMHPPrevIn = 0
	s.musicPCMHPPrevOut = 0
	s.musicPCMLPState = 0
	s.musicPCMActive = false
	s.musicPCMClosed = false
	s.musicPCMLoop = false
}

// ClearMusic stops and clears both tone-sequence and PCM music playback.
func (s *Source) ClearMusic() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.musicSeq = nil
	s.musicTickRate = 0
	s.musicSamplePos = 0
	s.musicLoop = false
	s.musicPCM = nil
	s.musicPCMPos = 0
	s.musicPCMRate = 0
	s.musicPCMPhase = 0
	s.musicPCMTarget = 0
	s.musicPCMTargetOK = false
	s.musicPCMError = 0
	s.musicPCMEnv = 0
	s.musicPCMAGain = 1
	s.musicPCMHPPrevIn = 0
	s.musicPCMHPPrevOut = 0
	s.musicPCMLPState = 0
	s.musicPCMActive = false
	s.musicPCMClosed = false
	s.musicPCMLoop = false
}

// SetMusicPCM loads a complete PCM music buffer.
//
// Input must be stereo signed 16-bit little-endian PCM. The data is consumed
// through the package's speaker encoder at DefaultPCMUpdateRate.
func (s *Source) SetMusicPCM(pcm []byte, sampleRate int, loop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.musicPCM = append([]byte(nil), pcm...)
	s.rate = sampleRate
	s.musicPCMRate = DefaultPCMUpdateRate
	s.musicPCMLoop = loop
	s.musicPCMPos = 0
	s.musicPCMPhase = 0
	s.musicPCMTarget = 0
	s.musicPCMTargetOK = false
	s.musicPCMError = 0
	s.musicPCMEnv = 0
	s.musicPCMAGain = 1
	s.musicPCMHPPrevIn = 0
	s.musicPCMHPPrevOut = 0
	s.musicPCMLPState = 0
	s.musicPCMActive = true
	s.musicPCMClosed = true
}

// BeginMusicPCM starts incremental PCM music streaming.
//
// Subsequent data should be appended with AppendMusicPCM and finalized with
// FinishMusicPCM.
func (s *Source) BeginMusicPCM(sampleRate int, loop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rate = sampleRate
	s.musicPCM = s.musicPCM[:0]
	s.musicPCMPos = 0
	s.musicPCMRate = DefaultPCMUpdateRate
	s.musicPCMPhase = 0
	s.musicPCMTarget = 0
	s.musicPCMTargetOK = false
	s.musicPCMError = 0
	s.musicPCMEnv = 0
	s.musicPCMAGain = 1
	s.musicPCMHPPrevIn = 0
	s.musicPCMHPPrevOut = 0
	s.musicPCMLPState = 0
	s.musicPCMActive = true
	s.musicPCMClosed = false
	s.musicPCMLoop = loop
}

// AppendMusicPCM queues more stereo signed 16-bit little-endian PCM data for
// an active incremental PCM music stream.
func (s *Source) AppendMusicPCM(pcm []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(pcm) == 0 {
		return
	}
	s.compactMusicPCMLocked(false)
	s.musicPCM = append(s.musicPCM, pcm...)
	s.musicPCMActive = true
}

// FinishMusicPCM marks an incremental PCM music stream as complete.
func (s *Source) FinishMusicPCM() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.musicPCMClosed = true
}

// BufferedMusicPCMBytes reports the number of unread PCM bytes still queued.
func (s *Source) BufferedMusicPCMBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compactMusicPCMLocked(false)
	if len(s.musicPCM) == 0 {
		return 0
	}
	pos := s.musicPCMPos * 4
	if pos >= len(s.musicPCM) {
		return 0
	}
	return len(s.musicPCM) - pos
}

// MusicPCMIsActive reports whether PCM music playback is still active.
func (s *Source) MusicPCMIsActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.musicPCMActive
}

// MusicIsActive reports whether any music source is currently active.
func (s *Source) MusicIsActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.musicPCMActive || len(s.musicSeq) > 0
}

// Read renders stereo 16-bit little-endian PCM into p.
//
// Source implements io.Reader. Output is written as interleaved left/right
// samples, 4 bytes per frame.
func (s *Source) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frames := len(p) / 4
	if frames == 0 {
		return 0, nil
	}
	written := 0
	for i := 0; i < frames; i++ {
		tone, direct, directDrive := s.currentToneLocked()
		if !tone.Active && !direct && !s.hasPlaybackLocked() {
			for j := written; j < len(p); j++ {
				p[j] = 0
			}
			if written == 0 {
				return 0, io.EOF
			}
			return len(p), io.EOF
		}
		var pitOut float64
		if direct {
			if !s.lastDirect {
				s.pitPhase = 0
				s.lastDivisor = 0
				s.lastActive = false
			}
			pitOut = directDrive
			s.lastDirect = true
		} else {
			divisorNow := tone.ToneDivisor()
			if tone.Active != s.lastActive || divisorNow != s.lastDivisor || s.lastDirect {
				s.pitPhase = 0
				s.lastDivisor = divisorNow
				s.lastActive = tone.Active
			}
			if tone.Active {
				divisor := float64(divisorNow)
				if divisor > 0 {
					highClocks := math.Ceil(divisor / 2.0)
					if s.pitPhase < highClocks {
						pitOut = 1.0
					}
					s.pitPhase += float64(PITHz) / float64(s.rate)
					if s.pitPhase >= divisor {
						s.pitPhase = math.Mod(s.pitPhase, divisor)
					}
				}
			} else {
				s.pitPhase = 0
			}
			pitOut *= s.streamGain
			s.lastDirect = false
		}
		if s.variant == VariantClean {
			raw := pitOut * s.model.gain
			in := raw
			raw = cleanDCBlockAlpha * (s.dcPrevOut + in - s.dcPrevIn)
			s.dcPrevIn = in
			s.dcPrevOut = raw
			if raw > math.MaxInt16 {
				raw = math.MaxInt16
			} else if raw < math.MinInt16 {
				raw = math.MinInt16
			}
			v := int16(raw)
			binary.LittleEndian.PutUint16(p[written:written+2], uint16(v))
			binary.LittleEndian.PutUint16(p[written+2:written+4], uint16(v))
			written += 4
			continue
		}
		var rawVel float64
		if s.variant == VariantPiezo {
			signedIn := 0.0
			if direct {
				signedIn = math.Max(-1, math.Min(1, directDrive))
			} else if tone.Active {
				if pitOut > 0 {
					signedIn = 1.0
				} else {
					signedIn = -1.0
				}
			}
			s.lpState = piezoElectricalAlpha*s.lpState + (1-piezoElectricalAlpha)*signedIn
			coilCurrent := s.lpState
			s.lastPiezoIn = signedIn
			s.noiseState = s.noiseState*1664525 + 1013904223
			noise := (float64((s.noiseState>>16)&0xffff)/32767.5 - 1.0) * piezoNoiseMix
			s.driftPhase += 2 * math.Pi * piezoDriftHz / float64(s.rate)
			if s.driftPhase > 2*math.Pi {
				s.driftPhase -= 2 * math.Pi
			}
			drive := coilCurrent
			if drive >= 0 {
				drive = math.Tanh(drive * piezoDriveAsymPos)
			} else {
				drive = math.Tanh(drive * piezoDriveAsymNeg)
			}
			s.driveState = 0.995*s.driveState + 0.005*math.Abs(drive)
			f0 := piezoF0Hz*(1.0+0.004*math.Tanh(s.driveState)) + piezoDriftRangeHz*math.Sin(s.driftPhase)
			r1 := math.Exp(-math.Pi * f0 / (piezoQ * float64(s.rate)))
			w1 := 2 * math.Pi * f0 / float64(s.rate)
			y1 := 2*r1*math.Cos(w1)*s.resY1 - r1*r1*s.resY2 + (1-r1)*drive
			s.resY2 = s.resY1
			s.resY1 = y1
			f1 := piezoCabF0Hz + 36.0*math.Sin(s.driftPhase*0.61+0.4)
			r2 := math.Exp(-math.Pi * f1 / (piezoCabQ * float64(s.rate)))
			w2 := 2 * math.Pi * f1 / float64(s.rate)
			y2 := 2*r2*math.Cos(w2)*s.res2Y1 - r2*r2*s.res2Y2 + (1-r2)*drive
			s.res2Y2 = s.res2Y1
			s.res2Y1 = y2
			mainBand := y1 - s.resY2
			upperBand := y2 - s.res2Y2
			acF := piezoHoleF0Hz + 22.0*math.Sin(s.driftPhase*0.47+0.2)
			acR := math.Exp(-math.Pi * acF / (piezoHoleQ * float64(s.rate)))
			acW := 2 * math.Pi * acF / float64(s.rate)
			acDrive := mainBand + upperBand*0.35
			acY := 2*acR*math.Cos(acW)*s.acY1 - acR*acR*s.acY2 + (1-acR)*acDrive
			s.acY2 = s.acY1
			s.acY1 = acY
			holeBand := acY - s.acY2
			rawVel = mainBand*piezoPrimaryMix + holeBand*piezoHoleMix + upperBand*piezoSecondaryMix + noise
			s.hpState = piezoHPAlpha*s.hpState + (1-piezoHPAlpha)*rawVel
			rawVel -= s.hpState
			s.freqSmooth = 0.52*s.freqSmooth + 0.48*rawVel
			rawVel = s.freqSmooth
			if rawVel >= 0 {
				rawVel = rawVel / (1.0 + 0.24*rawVel)
			} else {
				rawVel = rawVel / (1.0 + 0.34*math.Abs(rawVel))
			}
			rawVel *= math.MaxInt16 * (piezoDriveGain * 4.0)
		} else {
			force := pitOut * spkDrive
			accel := force - spkK*s.disp - spkD*s.vel
			s.vel += accel
			s.disp += s.vel
			rawVel = s.vel * spkGain
		}
		hpOut := rawVel
		if s.variant == VariantSmallSpeaker {
			hp1 := spkHPAlpha * (s.hp1Out + rawVel - s.hp1Prev)
			s.hp1Prev = rawVel
			s.hp1Out = hp1
			hp2 := spkHPAlpha * (s.hp2Out + hp1 - s.hp2Prev)
			s.hp2Prev = hp1
			s.hp2Out = hp2
			hpOut = hp2
		} else if s.model.hpAlpha < 1.0 {
			hp1 := s.model.hpAlpha * (s.hp1Out + rawVel - s.hp1Prev)
			s.hp1Prev = rawVel
			s.hp1Out = hp1
			hp2 := s.model.hpAlpha * (s.hp2Out + hp1 - s.hp2Prev)
			s.hp2Prev = hp1
			s.hp2Out = hp2
			hpOut = hp2
		}
		raw := hpOut
		if s.variant == VariantSmallSpeaker {
			wet := s.reverb.process(hpOut)
			raw = hpOut + wet*spkReverbMix
		} else if s.model.reverbMix > 0 {
			wet := s.reverb.process(hpOut)
			raw = hpOut + wet*s.model.reverbMix
		}
		if s.model.coneRadius == 0 && s.variant != VariantPiezo {
			in := raw
			raw = cleanDCBlockAlpha * (s.dcPrevOut + in - s.dcPrevIn)
			s.dcPrevIn = in
			s.dcPrevOut = raw
		}
		v := int16(math.Tanh(raw/math.MaxInt16) * math.MaxInt16)
		binary.LittleEndian.PutUint16(p[written:written+2], uint16(v))
		binary.LittleEndian.PutUint16(p[written+2:written+4], uint16(v))
		written += 4
	}
	return written, nil
}

// Seek repositions playback within the currently loaded render window.
//
// Offsets are measured in output bytes, matching the stream exposed by Read.
func (s *Source) Seek(offset int64, whence int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := int64(s.totalSamplesLocked()) * 4
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		cur := s.effectSamplePos
		if len(s.effectSeq) == 0 {
			cur = s.musicSamplePos
		}
		abs = int64(cur)*4 + offset
	case io.SeekEnd:
		abs = total + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if abs < 0 {
		abs = 0
	}
	s.effectSamplePos = int(abs / 4)
	s.musicSamplePos = int(abs / 4)
	s.resetStateLocked()
	return abs, nil
}

func (s *Source) currentToneLocked() (Tone, bool, float64) {
	if s.musicPCMActive {
		if drive, ok := s.nextMusicPCMDriveLocked(); ok {
			return Tone{}, true, drive
		}
		return Tone{}, true, 0
	}
	if len(s.effectSeq) > 0 && len(s.musicSeq) > 0 {
		effectTone, effectOK, _ := s.nextEffectToneLocked()
		musicTone, musicOK := s.nextMusicToneLocked()
		if effectOK && musicOK {
			if !effectTone.Active {
				return musicTone, false, 0
			}
			if !musicTone.Active {
				return effectTone, false, 0
			}
			choice := toneMixPattern[int((s.toneMixSample/uint64(toneInterleaveHoldSamples(effectTone, musicTone)))%uint64(len(toneMixPattern)))]
			s.toneMixSample++
			if choice == 0 {
				return effectTone, false, 0
			}
			return musicTone, false, 0
		}
		if effectOK {
			return effectTone, false, 0
		}
		if musicOK {
			return musicTone, false, 0
		}
		s.pitPhase = 0
		return Tone{}, false, 0
	}
	if tone, ok, _ := s.nextEffectToneLocked(); ok {
		return tone, false, 0
	}
	if tone, ok := s.nextMusicToneLocked(); ok {
		return tone, false, 0
	}
	s.pitPhase = 0
	return Tone{}, false, 0
}

func (s *Source) nextEffectToneLocked() (Tone, bool, bool) {
	total := s.effectTotalSamplesLocked()
	if total <= 0 || s.effectSamplePos >= total {
		s.effectSeq = nil
		s.effectSamplePos = 0
		return Tone{}, false, false
	}
	tone := toneAtSample(s.effectSeq, s.rate, s.effectTickRate, s.effectSamplePos)
	s.effectSamplePos++
	if s.effectSamplePos >= total {
		s.effectSeq = nil
		s.effectSamplePos = 0
	}
	return tone, tone.Active, false
}

func (s *Source) nextMusicToneLocked() (Tone, bool) {
	total := s.musicTotalSamplesLocked()
	if total <= 0 {
		s.musicSeq = nil
		s.musicSamplePos = 0
		return Tone{}, false
	}
	if s.musicSamplePos >= total {
		if !s.musicLoop {
			s.musicSeq = nil
			s.musicSamplePos = 0
			return Tone{}, false
		}
		s.musicSamplePos = 0
	}
	tone := toneAtSample(s.musicSeq, s.rate, s.musicTickRate, s.musicSamplePos)
	s.musicSamplePos++
	if s.musicLoop && s.musicSamplePos >= total {
		s.musicSamplePos = 0
	}
	return tone, true
}

func (s *Source) nextMusicPCMDriveLocked() (float64, bool) {
	target, ok := s.nextPreEncoderMixedTargetLocked()
	if !ok {
		return 0, false
	}
	target = s.filterMusicPCMTargetLocked(target)
	absTarget := math.Abs(target)
	attack := agcBlendForMs(s.musicPCMRate, agcAttackMS)
	release := agcBlendForMs(s.musicPCMRate, agcReleaseMS)
	if absTarget > s.musicPCMEnv {
		s.musicPCMEnv += (absTarget - s.musicPCMEnv) * attack
	} else {
		s.musicPCMEnv += (absTarget - s.musicPCMEnv) * release
	}
	if s.musicPCMEnv < 1e-5 {
		s.musicPCMEnv = 1e-5
	}
	desiredGain := agcTarget / s.musicPCMEnv
	if desiredGain < agcMinGain {
		desiredGain = agcMinGain
	} else if desiredGain > agcMaxGain {
		desiredGain = agcMaxGain
	}
	gainBlend := agcBlendForMs(s.musicPCMRate, agcGainFallMS)
	if desiredGain > s.musicPCMAGain {
		gainBlend = agcBlendForMs(s.musicPCMRate, agcGainRiseMS)
	}
	s.musicPCMAGain += (desiredGain - s.musicPCMAGain) * gainBlend
	boosted := target * s.musicPCMAGain
	if boosted > 1 {
		boosted = 1
	} else if boosted < -1 {
		boosted = -1
	}
	if s.musicPCMError+boosted >= 0 {
		s.musicPCMError += boosted - 1
		return 1, true
	}
	s.musicPCMError += boosted + 1
	return -1, true
}

func (s *Source) nextPreEncoderMixedTargetLocked() (float64, bool) {
	target := 0.0
	haveTarget := false
	if musicTarget, ok := s.nextMusicPCMTargetLocked(); ok {
		target = musicTarget
		haveTarget = true
	}
	if effectDrive, ok := s.nextEffectMixedDriveLocked(); ok {
		if haveTarget {
			target = mixPreEncoderSignals(target, effectDrive)
		} else {
			target = effectDrive
		}
		haveTarget = true
	}
	if !haveTarget {
		return 0, false
	}
	if target > 1 {
		target = 1
	} else if target < -1 {
		target = -1
	}
	return target, true
}

func (s *Source) nextMusicPCMTargetLocked() (float64, bool) {
	total := s.musicPCMTotalSamplesLocked()
	if total <= 0 {
		if s.musicPCMClosed {
			s.musicPCMActive = false
		}
		return 0, false
	}
	if s.musicPCMRate <= 0 {
		s.musicPCMRate = DefaultPCMUpdateRate
	}
	if !s.musicPCMTargetOK {
		if !s.loadNextMusicPCMTargetLocked() {
			return 0, false
		}
	}
	step := float64(s.musicPCMRate) / float64(s.rate)
	if step <= 0 {
		step = 1
	}
	s.musicPCMPhase += step
	for s.musicPCMPhase >= 1 {
		s.musicPCMPhase -= 1
		if !s.advanceMusicPCMFrameLocked() {
			break
		}
	}
	target := s.musicPCMTarget
	if target > 1 {
		target = 1
	} else if target < -1 {
		target = -1
	}
	return target, true
}

func (s *Source) nextEffectMixedDriveLocked() (float64, bool) {
	total := s.effectTotalSamplesLocked()
	if total <= 0 || s.effectSamplePos >= total {
		s.effectSeq = nil
		s.effectSamplePos = 0
		s.effectMixPhase = 0
		s.effectMixDivisor = 0
		s.effectMixActive = false
		return 0, false
	}
	tone := toneAtSample(s.effectSeq, s.rate, s.effectTickRate, s.effectSamplePos)
	divisorNow := tone.ToneDivisor()
	if tone.Active != s.effectMixActive || divisorNow != s.effectMixDivisor {
		s.effectMixPhase = 0
		s.effectMixDivisor = divisorNow
		s.effectMixActive = tone.Active
	}
	drive := 0.0
	if tone.Active && divisorNow > 0 {
		divisor := float64(divisorNow)
		highClocks := math.Ceil(divisor / 2.0)
		drive = -1.0
		if s.effectMixPhase < highClocks {
			drive = 1.0
		}
		s.effectMixPhase += float64(PITHz) / float64(s.rate)
		if s.effectMixPhase >= divisor {
			s.effectMixPhase = math.Mod(s.effectMixPhase, divisor)
		}
	}
	s.effectSamplePos++
	if s.effectSamplePos >= total {
		s.effectSeq = nil
		s.effectSamplePos = 0
		s.effectMixPhase = 0
		s.effectMixDivisor = 0
		s.effectMixActive = false
	}
	return drive, true
}

func (s *Source) hasPlaybackLocked() bool {
	return len(s.effectSeq) > 0 || len(s.musicSeq) > 0 || len(s.musicPCM) > 0 || s.musicPCMActive
}

func (s *Source) loadNextMusicPCMTargetLocked() bool {
	total := s.musicPCMTotalSamplesLocked()
	if total <= 0 || s.musicPCMPos >= total {
		if !s.musicPCMLoop {
			if s.musicPCMClosed {
				s.musicPCMTargetOK = false
				s.musicPCMActive = false
				return false
			}
			return s.musicPCMTargetOK
		}
		s.musicPCMPos = 0
	}
	base := s.musicPCMPos * 4
	if base+3 >= len(s.musicPCM) {
		return false
	}
	l := int16(binary.LittleEndian.Uint16(s.musicPCM[base : base+2]))
	r := int16(binary.LittleEndian.Uint16(s.musicPCM[base+2 : base+4]))
	s.musicPCMTarget = float64(int(l)+int(r)) / (2.0 * float64(math.MaxInt16))
	s.musicPCMTargetOK = true
	return true
}

func (s *Source) advanceMusicPCMFrameLocked() bool {
	total := s.musicPCMTotalSamplesLocked()
	if total <= 0 {
		if s.musicPCMClosed {
			s.musicPCMTargetOK = false
			s.musicPCMActive = false
			return false
		}
		return s.musicPCMTargetOK
	}
	nextPos := s.musicPCMPos + 1
	if nextPos < total {
		s.musicPCMPos = nextPos
		s.musicPCMTargetOK = false
		return s.loadNextMusicPCMTargetLocked()
	}
	if s.musicPCMLoop {
		s.musicPCMPos = 0
		s.musicPCMTargetOK = false
		return s.loadNextMusicPCMTargetLocked()
	}
	if s.musicPCMClosed {
		s.musicPCMPos = nextPos
		s.musicPCMTargetOK = false
		s.musicPCMActive = false
		return false
	}
	return true
}

func (s *Source) compactMusicPCMLocked(force bool) {
	if len(s.musicPCM) == 0 || s.musicPCMPos <= 0 {
		return
	}
	consumedBytes := s.musicPCMPos * 4
	if consumedBytes <= 0 {
		return
	}
	if !force {
		if consumedBytes < pcmCompactThresholdBytes {
			return
		}
		if consumedBytes*2 < len(s.musicPCM) {
			return
		}
	}
	if consumedBytes >= len(s.musicPCM) {
		s.musicPCM = s.musicPCM[:0]
		s.musicPCMPos = 0
		return
	}
	remaining := len(s.musicPCM) - consumedBytes
	copy(s.musicPCM[:remaining], s.musicPCM[consumedBytes:])
	s.musicPCM = s.musicPCM[:remaining]
	s.musicPCMPos = 0
}

func (s *Source) filterMusicPCMTargetLocked(v float64) float64 {
	if s.musicPCMRate <= 0 {
		return v
	}
	hpAlpha := highPassAlphaForHz(float64(s.musicPCMRate), pcmHighPassHz)
	hpOut := hpAlpha * (s.musicPCMHPPrevOut + v - s.musicPCMHPPrevIn)
	s.musicPCMHPPrevIn = v
	s.musicPCMHPPrevOut = hpOut
	lpAlpha := lowPassBlendForHz(float64(s.musicPCMRate), pcmLowPassHz)
	s.musicPCMLPState += (hpOut - s.musicPCMLPState) * lpAlpha
	return s.musicPCMLPState
}

func (s *Source) resetStateLocked() {
	s.pitPhase = 0
	s.lastDivisor = 0
	s.lastActive = false
	s.effectMixPhase = 0
	s.effectMixDivisor = 0
	s.effectMixActive = false
	s.toneMixSample = 0
	s.vel = 0
	s.disp = 0
	s.hp1Prev = 0
	s.hp1Out = 0
	s.hp2Prev = 0
	s.hp2Out = 0
	s.dcPrevIn = 0
	s.dcPrevOut = 0
	s.lpState = 0
	s.envState = 0
	s.driveState = 0
	s.resY1 = 0
	s.resY2 = 0
	s.res2Y1 = 0
	s.res2Y2 = 0
	s.acY1 = 0
	s.acY2 = 0
	s.hpState = 0
	s.driftPhase = 0
	s.freqSmooth = 0
	s.lastFreq = 0
	s.lastPiezoIn = 0
	s.piezoTickAge = 0
	s.noiseState = 1
	s.reverb.reset()
}

var toneMixPattern = []int{0, 1, 0}

func toneInterleaveHoldSamples(effectTone Tone, musicTone Tone) int {
	holdSeconds := 1.0 / toneInterleaveTargetHz
	lowestHz := math.MaxFloat64
	for _, tone := range [...]Tone{effectTone, musicTone} {
		divisor := tone.ToneDivisor()
		if !tone.Active || divisor == 0 {
			continue
		}
		hz := float64(PITHz) / float64(divisor)
		if hz > 0 && hz < lowestHz {
			lowestHz = hz
		}
	}
	if lowestHz != math.MaxFloat64 {
		minSeconds := toneInterleaveMinCycles / lowestHz
		if minSeconds > holdSeconds {
			holdSeconds = minSeconds
		}
	}
	hold := int(math.Ceil(holdSeconds * DefaultOutputSampleRate))
	if hold < 1 {
		return 1
	}
	return hold
}

func normalizeInterleaveTickRate(a int, b int) int {
	rate := max(normalizeTickRate(a), normalizeTickRate(b))
	if rate < 560 {
		rate = 560
	}
	return rate
}

func normalizeTickRate(rate int) int {
	if rate <= 0 {
		return DefaultTickRate
	}
	return rate
}

func prepareEffectSeqForEmulatedMix(seq []Tone, tickRate int) ([]Tone, int) {
	tickRate = normalizeTickRate(tickRate)
	if len(seq) == 0 {
		return nil, tickRate
	}
	if tickRate >= DefaultMixTickRate || DefaultMixTickRate%tickRate != 0 {
		return append([]Tone(nil), seq...), tickRate
	}
	repeat := DefaultMixTickRate / tickRate
	if repeat <= 1 {
		return append([]Tone(nil), seq...), tickRate
	}
	out := make([]Tone, 0, len(seq)*repeat)
	for _, tone := range seq {
		for i := 0; i < repeat; i++ {
			out = append(out, tone)
		}
	}
	return out, DefaultMixTickRate
}

func toneAtSample(seq []Tone, rate int, tickRate int, samplePos int) Tone {
	if len(seq) == 0 || rate <= 0 {
		return Tone{}
	}
	tickRate = normalizeTickRate(tickRate)
	samplesPerTick := float64(rate) / float64(tickRate)
	tickIdx := int(float64(samplePos) / samplesPerTick)
	if tickIdx >= len(seq) {
		tickIdx = len(seq) - 1
	}
	return seq[tickIdx]
}

func totalSamplesForToneSeq(seqLen int, sampleRate int, tickRate int) int {
	if seqLen <= 0 || sampleRate <= 0 {
		return 0
	}
	tickRate = normalizeTickRate(tickRate)
	return int(math.Round(float64(seqLen) * float64(sampleRate) / float64(tickRate)))
}

// InterleaveSequences combines effect and music tone sequences into one tone
// stream that approximates how a single physical speaker would be time-shared.
//
// It returns the merged sequence and the output tick rate used for that sequence.
func InterleaveSequences(effectSeq []Tone, effectTickRate int, musicSeq []Tone, musicTickRate int) ([]Tone, int) {
	if len(effectSeq) == 0 {
		return append([]Tone(nil), musicSeq...), normalizeTickRate(musicTickRate)
	}
	if len(musicSeq) == 0 {
		return append([]Tone(nil), effectSeq...), normalizeTickRate(effectTickRate)
	}
	outTickRate := normalizeInterleaveTickRate(effectTickRate, musicTickRate)
	total := max(totalTicksAtRate(len(effectSeq), effectTickRate, outTickRate), totalTicksAtRate(len(musicSeq), musicTickRate, outTickRate))
	out := make([]Tone, 0, total)
	var mixTick uint64
	for i := 0; i < total; i++ {
		effectTone, effectOK := toneAtTick(effectSeq, effectTickRate, outTickRate, i)
		musicTone, musicOK := toneAtTick(musicSeq, musicTickRate, outTickRate, i)
		switch {
		case effectOK && musicOK:
			if !effectTone.Active {
				out = append(out, musicTone)
				continue
			}
			if !musicTone.Active {
				out = append(out, effectTone)
				continue
			}
			hold := toneInterleaveHoldTicks(effectTone, musicTone, outTickRate)
			choice := toneMixPattern[int((mixTick/uint64(hold))%uint64(len(toneMixPattern)))]
			mixTick++
			if choice == 0 {
				out = append(out, effectTone)
			} else {
				out = append(out, musicTone)
			}
		case effectOK:
			out = append(out, effectTone)
		case musicOK:
			out = append(out, musicTone)
		default:
			out = append(out, Tone{})
		}
	}
	return out, outTickRate
}

// RenderSequenceToPCM renders one tone sequence to stereo PCM samples.
//
// The returned slice contains interleaved left/right 16-bit samples.
func RenderSequenceToPCM(seq []Tone, tickRate int, variant Variant) ([]int16, error) {
	if len(seq) == 0 {
		return nil, nil
	}
	src := NewSource(variant)
	src.Load(seq, DefaultOutputSampleRate)
	if tickRate > 0 {
		src.mu.Lock()
		src.effectTickRate = tickRate
		src.mu.Unlock()
	}
	return renderSourceToPCM(src)
}

// RenderMixedSequencesToPCM interleaves effect and music tone sequences, then
// renders the result to stereo PCM samples.
func RenderMixedSequencesToPCM(effectSeq []Tone, effectTickRate int, musicSeq []Tone, musicTickRate int, variant Variant) ([]int16, error) {
	if len(effectSeq) == 0 && len(musicSeq) == 0 {
		return nil, nil
	}
	seq, tickRate := InterleaveSequences(effectSeq, effectTickRate, musicSeq, musicTickRate)
	return RenderSequenceToPCM(seq, tickRate, variant)
}

func renderSourceToPCM(src *Source) ([]int16, error) {
	if src == nil {
		return nil, nil
	}
	totalFrames := src.TotalSamples()
	if totalFrames <= 0 {
		return nil, nil
	}
	buf := make([]byte, totalFrames*4)
	n, err := io.ReadFull(src, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("render pc speaker pcm: %w", err)
	}
	buf = buf[:n]
	if len(buf)%2 != 0 {
		buf = buf[:len(buf)-1]
	}
	out := make([]int16, len(buf)/2)
	for i := 0; i+1 < len(buf); i += 2 {
		out[i/2] = int16(binary.LittleEndian.Uint16(buf[i : i+2]))
	}
	return out, nil
}

func toneAtTick(seq []Tone, seqTickRate int, outTickRate int, tick int) (Tone, bool) {
	if len(seq) == 0 || tick < 0 {
		return Tone{}, false
	}
	seqTickRate = normalizeTickRate(seqTickRate)
	outTickRate = normalizeTickRate(outTickRate)
	idx := int((int64(tick) * int64(seqTickRate)) / int64(outTickRate))
	if idx < 0 || idx >= len(seq) {
		return Tone{}, false
	}
	return seq[idx], true
}

func totalTicksAtRate(seqLen int, seqTickRate int, outTickRate int) int {
	if seqLen <= 0 {
		return 0
	}
	seqTickRate = normalizeTickRate(seqTickRate)
	outTickRate = normalizeTickRate(outTickRate)
	return int(math.Ceil(float64(seqLen) * float64(outTickRate) / float64(seqTickRate)))
}

func toneInterleaveHoldTicks(effectTone Tone, musicTone Tone, tickRate int) int {
	if tickRate <= 0 {
		tickRate = 560
	}
	holdSeconds := 1.0 / toneInterleaveTargetHz
	lowestHz := math.MaxFloat64
	for _, tone := range [...]Tone{effectTone, musicTone} {
		divisor := tone.ToneDivisor()
		if !tone.Active || divisor == 0 {
			continue
		}
		hz := float64(PITHz) / float64(divisor)
		if hz > 0 && hz < lowestHz {
			lowestHz = hz
		}
	}
	if lowestHz != math.MaxFloat64 {
		minSeconds := toneInterleaveMinCycles / lowestHz
		if minSeconds > holdSeconds {
			holdSeconds = minSeconds
		}
	}
	hold := int(math.Ceil(holdSeconds * float64(tickRate)))
	if hold < 1 {
		return 1
	}
	return hold
}

func agcBlendForMs(rate int, ms float64) float64 {
	if rate <= 0 || ms <= 0 {
		return 1
	}
	return 1 - math.Exp(-1000.0/(float64(rate)*ms))
}

func highPassAlphaForHz(rate float64, cutoffHz float64) float64 {
	if rate <= 0 || cutoffHz <= 0 {
		return 1
	}
	rc := 1.0 / (2 * math.Pi * cutoffHz)
	dt := 1.0 / rate
	return rc / (rc + dt)
}

func lowPassBlendForHz(rate float64, cutoffHz float64) float64 {
	if rate <= 0 || cutoffHz <= 0 {
		return 1
	}
	return 1 - math.Exp(-2*math.Pi*cutoffHz/rate)
}

func mixPreEncoderSignals(a float64, b float64) float64 { return (a + b) * 0.5 }

func clampVolume(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
