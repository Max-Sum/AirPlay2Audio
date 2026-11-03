//go:build windows && !(amd64 || arm64)

package codec

import "errors"

var errNoWASAPI = errors.New("WASAPI output needs a 64-bit build")

type wasapiUnsupported struct{}

func newWASAPIStream(*softVolume) Stream            { return wasapiUnsupported{} }
func (wasapiUnsupported) Init(Source) error         { return errNoWASAPI }
func (wasapiUnsupported) Start() error              { return errNoWASAPI }
func (wasapiUnsupported) Stop() error               { return nil }
func (wasapiUnsupported) Close() error              { return nil }
func (wasapiUnsupported) SetVolume(float64) error   { return nil }
func wasapiDevices() ([]Device, error)              { return nil, errNoWASAPI }
