//go:build !windows

package audio

// FDK AAC decodes 7.1.
const (
	decodes71     = true
	audioFormat71 = audioFormatAAC48000x71
)
