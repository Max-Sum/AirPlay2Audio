//go:build windows

package audio

// The Windows AAC decoder (Media Foundation) decodes at most 5.1, so 7.1 is
// not advertised and its packets are skipped.
const (
	decodes71     = false
	audioFormat71 = 0
)
