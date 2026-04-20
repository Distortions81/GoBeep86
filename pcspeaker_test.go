package gobeep86

import (
	"encoding/binary"
	"io"
	"math"
	"testing"
)

func TestVariantsProducePCM(t *testing.T) {
	seq := make([]Tone, 140)
	for i := range seq {
		seq[i] = Tone{Active: true, Divisor: 96}
	}
	check := func(t *testing.T, variant Variant, minPeak int) {
		t.Helper()
		src := NewSource(variant)
		src.Load(seq, DefaultOutputSampleRate)
		buf := make([]byte, 4096)
		maxAbs := 0
		for {
			n, err := src.Read(buf)
			for i := 0; i+3 < n; i += 4 {
				v := int(int16(buf[i]) | int16(buf[i+1])<<8)
				if v < 0 {
					v = -v
				}
				if v > maxAbs {
					maxAbs = v
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("read failed for %s: %v", variant.String(), err)
			}
		}
		if maxAbs < minPeak {
			t.Fatalf("%s peak too low: got %d want >= %d", variant.String(), maxAbs, minPeak)
		}
	}
	check(t, VariantClean, 1000)
	check(t, VariantSmallSpeaker, 1000)
	check(t, VariantPiezo, 1000)
}

func TestMusicPCMUnderrunHoldsLastTarget(t *testing.T) {
	src := NewSource(VariantSmallSpeaker)
	src.BeginMusicPCM(DefaultOutputSampleRate, false)
	src.mu.Lock()
	src.musicPCMRate = DefaultPCMUpdateRate
	src.musicPCM = make([]byte, 4)
	binary.LittleEndian.PutUint16(src.musicPCM[0:2], uint16(int16(12000)))
	binary.LittleEndian.PutUint16(src.musicPCM[2:4], uint16(int16(12000)))
	src.mu.Unlock()

	src.mu.Lock()
	first, ok := src.nextMusicPCMDriveLocked()
	src.mu.Unlock()
	if !ok {
		t.Fatal("expected first PCM drive sample")
	}
	for i := 0; i < 6; i++ {
		src.mu.Lock()
		drive, ok := src.nextMusicPCMDriveLocked()
		src.mu.Unlock()
		if !ok {
			t.Fatalf("unexpected underrun silence at step %d", i)
		}
		if drive != first {
			t.Fatalf("expected held drive during underrun, got %v want %v", drive, first)
		}
	}
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint16(buf[0:2], uint16(int16(12000)))
	binary.LittleEndian.PutUint16(buf[2:4], uint16(int16(12000)))
	src.AppendMusicPCM(buf)
	src.mu.Lock()
	_, ok = src.nextMusicPCMDriveLocked()
	src.mu.Unlock()
	if !ok {
		t.Fatal("expected resumed PCM drive after append")
	}
}

func TestInterleaveSequencesReturnsOutput(t *testing.T) {
	out, tickRate := InterleaveSequences([]Tone{{Active: true, Divisor: 20}}, 140, []Tone{{Active: true, Divisor: 96}}, 140)
	if len(out) == 0 {
		t.Fatal("expected interleaved output")
	}
	if tickRate <= 0 {
		t.Fatalf("tickRate=%d want > 0", tickRate)
	}
}

func TestRenderSequenceToPCMReturnsSamples(t *testing.T) {
	pcm, err := RenderSequenceToPCM([]Tone{{Active: true, Divisor: 96}, {Active: true, Divisor: 96}}, 140, VariantSmallSpeaker)
	if err != nil {
		t.Fatalf("RenderSequenceToPCM() error = %v", err)
	}
	if len(pcm) == 0 {
		t.Fatal("expected PCM output")
	}
	peak := 0
	for _, v := range pcm {
		a := int(math.Abs(float64(v)))
		if a > peak {
			peak = a
		}
	}
	if peak == 0 {
		t.Fatal("expected non-silent PCM")
	}
}
