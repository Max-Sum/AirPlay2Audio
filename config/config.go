package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

type Configuration struct {
	Volume     float64 `json:"sound-volume"`
	DeviceUUID string  `json:"device-uuid"`
	PulseSink  string  `json:"-"`
	DeviceName string  `json:"-"`
	Password   string  `json:"-"` // empty: no password
	dataDir    string
}

var Config = &Configuration{Volume: -10, DeviceUUID: uuid.NewString()}

func (c *Configuration) Load(dataDir string) error {
	c.dataDir = dataDir
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dataDir, "config.json")
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, c); err != nil {
			return fmt.Errorf("read receiver configuration: %w", err)
		}
		if _, err = uuid.Parse(c.DeviceUUID); err != nil {
			return fmt.Errorf("invalid stored receiver identity: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return c.Store()
}

func (c *Configuration) Store() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dataDir, "config.json"), data, 0600)
}
