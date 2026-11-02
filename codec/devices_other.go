//go:build !windows

package codec

import "fmt"

// Devices is only implemented for the Windows output.
func Devices() ([]Device, error) {
	return nil, fmt.Errorf("listing output devices is only supported on Windows")
}
