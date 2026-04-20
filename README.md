# GoBeep86

GoBeep86 helps Go programs play sound that feels like it is coming from an old PC speaker.

It is for projects that want classic computer beeps, simple music, or retro sound effects without sounding too clean or modern. A big part of that is the built-in physical speaker simulation: it can model either a small paper-cone speaker or a piezo-style speaker, along with the steel PC case around it. That adds the reverb, ringing, and boxy resonance that people associate with old machines instead of a flat electronic square wave.

You can use it to:

- play retro sound effects and simple music from Go code
- render PC-speaker-style audio to PCM samples for saving, mixing, or playback elsewhere
- choose between a clean direct sound and paper-speaker or piezo speaker models with steel-case reverb/ring simulation

It accepts either:

- tone sequences expressed as PIT divisors
- stereo signed 16-bit little-endian PCM that is re-driven through a simulated 1-bit speaker path

It can be used in two main ways:

- as a streaming `io.Reader` via `Source`
- as offline render helpers that return `[]int16` PCM

Important behavior up front:

- output is always 16-bit stereo little-endian PCM
- effect tones and music tones can share one simulated speaker through time interleaving
- PCM music can be mixed with tone effects before PC speaker encoding
- `VariantClean` is the dry/pass-through model
- `VariantSmallSpeaker` simulates a paper-cone PC speaker mounted in a steel case
- `VariantPiezo` simulates a brighter buzzer/piezo speaker coupled to the same resonant case behavior
- the non-clean variants include built-in case resonance/ringing rather than a plain dry square wave

If you just want the main entry points:

```go
src := gobeep86.NewSource(gobeep86.VariantSmallSpeaker)
src.Load(effectSeq, gobeep86.DefaultOutputSampleRate)

pcm, err := gobeep86.RenderSequenceToPCM(seq, tickRate, gobeep86.VariantSmallSpeaker)
```

## Features

- play effect and music tone streams through one `Source`
- stream already-encoded PCM music through the same 1-bit speaker path
- mix PCM music with tone-based sound effects before the PC speaker encoder
- loop music tone sequences or streamed PCM music
- seek within rendered output
- render sequences directly to `[]int16` PCM without setting up a live stream
- choose between multiple speaker models

## Core Types

### `Tone`

`Tone` is one tick of PC speaker state:

- `Active` controls whether the speaker gate is on
- `Divisor` is the PIT channel 2 divisor for that tick

Helpers:

- `ToneDivisor()` returns `0` for inactive tones
- `ToneFrequency()` converts an active divisor to Hz
- `PITDivisorForFrequency(freq)` converts Hz back to the nearest PIT divisor

### `Variant`

Available output models:

- `VariantClean`: direct square-wave style output with minimal coloration
- `VariantSmallSpeaker`: paper-cone PC speaker with steel-case resonance
- `VariantPiezo`: brighter small-buzzer / piezo model with steel-case resonance

String helpers:

- `variant.String()` returns `passthrough`, `paper-speaker`, or `small-buzzer`
- `ParseVariant(s)` accepts aliases such as `clean`, `none`, `piezo`, `tiny`, and `moving-iron`

## `Source` Playback

Create a source with:

```go
src := gobeep86.NewSource(gobeep86.VariantSmallSpeaker)
```

`Source` implements `io.Reader` and emits 16-bit stereo little-endian PCM frames.

### Effect tones

For one-shot effects at the default tick rate:

```go
src.Load(seq, gobeep86.DefaultOutputSampleRate)
```

For effects that should use a specific tick rate and the library's emulated single-speaker mixing path:

```go
src.SetEffectMixed(seq, gobeep86.DefaultOutputSampleRate, tickRate)
```

### Music tones

Load a tone sequence as music:

```go
src.SetMusic(seq, gobeep86.DefaultOutputSampleRate, tickRate, loop)
```

Clear it with:

```go
src.ClearMusic()
```

### PCM music

Load a complete stereo PCM buffer:

```go
src.SetMusicPCM(pcmBytes, gobeep86.DefaultOutputSampleRate, loop)
```

Or stream PCM incrementally:

```go
src.BeginMusicPCM(gobeep86.DefaultOutputSampleRate, loop)
src.AppendMusicPCM(chunk)
src.FinishMusicPCM()
```

PCM input is expected to be stereo signed 16-bit little-endian frames. Internally it is resampled to the PC speaker control rate and encoded back into the simulated 1-bit output path.

Status helpers:

- `BufferedMusicPCMBytes()` reports unread queued PCM bytes
- `MusicPCMIsActive()` reports whether PCM playback is still running
- `MusicIsActive()` reports whether either tone music or PCM music is active

### Output control

- `SetVariant(v)` switches speaker model
- `SetGain(v)` applies stream gain before speaker modeling
- `TotalSamples()` reports the remaining render length for loaded content
- `Seek(offset, whence)` repositions playback within the current render window

## Offline Rendering Helpers

Render one sequence:

```go
pcm, err := gobeep86.RenderSequenceToPCM(seq, tickRate, gobeep86.VariantSmallSpeaker)
```

Render interleaved effect and music tone sequences:

```go
pcm, err := gobeep86.RenderMixedSequencesToPCM(effectSeq, effectTickRate, musicSeq, musicTickRate, gobeep86.VariantSmallSpeaker)
```

If you only need the interleaved tone data, use:

```go
mixed, tickRate := gobeep86.InterleaveSequences(effectSeq, effectTickRate, musicSeq, musicTickRate)
```

## Settings And Defaults

Package defaults:

- `DefaultTickRate = 140`
- `DefaultMixTickRate = 280`
- `DefaultPCMUpdateRate = 11025`
- `DefaultOutputSampleRate = 44100`
- `PITHz = 1193181`

Interleave tuning:

- `SetInterleaveHz(hz)` controls how quickly `InterleaveSequences` and live effect/music tone sharing switch between active tones
- values are clamped to `10..1000` Hz
- the default interleave target is `140` Hz

Tick-rate behavior:

- `Load()` uses `DefaultTickRate`
- `SetEffectMixed()` may upsample lower effect tick rates to `DefaultMixTickRate` when possible
- `SetMusicPCM()` and `BeginMusicPCM()` use `DefaultPCMUpdateRate`
- rendering and streaming output default to `DefaultOutputSampleRate`

## Notes

- inactive tones are represented by `Tone{}` or any `Tone` with `Active: false`
- when both effect tones and music tones are present, they are interleaved to emulate a single physical speaker
- PCM music is filtered, gain-ridden, and 1-bit encoded so it behaves like speaker drive rather than clean line-level audio
