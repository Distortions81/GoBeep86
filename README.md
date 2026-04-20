# GoBeep86

GoBeep86 is a generic PC speaker simulation library for Go projects.

This library contains:
- tone sequences expressed as PIT divisors
- a streaming PCM source for PC speaker simulation
- offline PCM rendering helpers
- sequence interleaving for shared single-speaker playback

It does not contain any Doom-specific parsing, WAD handling, or music conversion logic.
