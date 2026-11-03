package codec

import (
	"fmt"
	"strings"
)

// OutputConfig selects the output device and the format written to it. The
// player produces stereo at the rate and sample size of the stream being
// played; the output follows it, or converts it to a fixed format.
type OutputConfig struct {
	// Device is empty for the system default device, a number for a device
	// index from Devices, or otherwise a case-insensitive part of its name.
	Device string
	Bits   int // 16, 24 or 32; 0 follows the stream
	Rate   int // 44100, 48000, 88200, 96000, 176400 or 192000; 0 follows the stream
	// Dither is "auto" (only for 16-bit output whose samples were scaled,
	// resampled or had more than 16 bits), "on" or "off".
	Dither string
	// API is the Windows output API: "wasapi" (default; falls back to
	// waveOut when it cannot be opened) or "waveout".
	API string
	// BufferMs is how much audio WASAPI keeps queued at the device, in
	// milliseconds; 0 uses DefaultBufferMs. Less is less delay, but too
	// little lets a busy computer run the device dry.
	BufferMs int
}

// DefaultBufferMs is the WASAPI queue kept by default: two periods of the
// usual 10 ms Windows audio engine period.
const DefaultBufferMs = 20

// Output is read when a stream opens its device.
var Output = OutputConfig{Dither: "auto", API: "wasapi", BufferMs: DefaultBufferMs}

// Resolve returns the format written to the device while the player
// produces audio in format in.
func (c OutputConfig) Resolve(in Format) Format {
	out := Format{Rate: c.Rate, Bits: c.Bits}
	if out.Rate == 0 {
		out.Rate = in.Rate
	}
	if out.Bits == 0 {
		out.Bits = 16
		if in.Bits > 16 {
			out.Bits = 24
		}
	}
	return out
}

func (c OutputConfig) Validate() error {
	switch c.Bits {
	case 0, 16, 24, 32:
	default:
		return fmt.Errorf("output bits must be 0 (follow the stream), 16, 24 or 32")
	}
	switch c.Rate {
	case 0, 44100, 48000, 88200, 96000, 176400, 192000:
	default:
		return fmt.Errorf("output rate must be 0 (follow the stream), 44100, 48000, 88200, 96000, 176400 or 192000")
	}
	switch strings.ToLower(c.Dither) {
	case "auto", "on", "off":
	default:
		return fmt.Errorf("dither must be auto, on or off")
	}
	switch strings.ToLower(c.API) {
	case "", "wasapi", "waveout":
	default:
		return fmt.Errorf("output API must be wasapi or waveout")
	}
	if c.BufferMs != 0 && (c.BufferMs < 5 || c.BufferMs > 500) {
		return fmt.Errorf("output buffer must be between 5 and 500 ms")
	}
	return nil
}

// Device is an output device as listed by Devices.
type Device struct {
	Index int
	Name  string
}

// matchDevice resolves a -device value against the listed devices.
func matchDevice(spec string, devices []Device) (Device, error) {
	spec = strings.TrimSpace(spec)
	var index int
	if _, err := fmt.Sscanf(spec, "%d", &index); err == nil && fmt.Sprint(index) == spec {
		for _, d := range devices {
			if d.Index == index {
				return d, nil
			}
		}
		return Device{}, fmt.Errorf("no output device with index %d", index)
	}
	var found []Device
	for _, d := range devices {
		if strings.EqualFold(d.Name, spec) {
			return d, nil
		}
		if strings.Contains(strings.ToLower(d.Name), strings.ToLower(spec)) {
			found = append(found, d)
		}
	}
	switch len(found) {
	case 0:
		return Device{}, fmt.Errorf("no output device matches %q", spec)
	case 1:
		return found[0], nil
	}
	// A name saved from waveOut is the first 31 characters of the full name.
	var prefixed []Device
	for _, d := range found {
		if strings.HasPrefix(strings.ToLower(d.Name), strings.ToLower(spec)) {
			prefixed = append(prefixed, d)
		}
	}
	if len(prefixed) == 1 {
		return prefixed[0], nil
	}
	return Device{}, fmt.Errorf("%d output devices match %q; use the index", len(found), spec)
}
