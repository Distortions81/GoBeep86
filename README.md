# GoBeep86

GoBeep86 is a PC speaker simulation library for Go projects.

This library contains:
- tone sequences expressed as PIT divisors
- a streaming PCM source for PC speaker simulation
- offline PCM rendering helpers
- sequence interleaving for shared single-speaker playback

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
- `VariantSmallSpeaker`: colored paper-cone PC speaker model
- `VariantPiezo`: brighter small-buzzer / piezo style model

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
