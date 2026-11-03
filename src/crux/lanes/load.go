package lanes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const maxConfigBytes = 1 << 20

func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("%w: cannot open lane configuration", ErrInvalid)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || stat.Size() > maxConfigBytes {
		return Config{}, fmt.Errorf("%w: lane configuration size", ErrInvalid)
	}
	return loadConfig(f)
}

func loadConfig(r io.Reader) (Config, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		return Config{}, fmt.Errorf("%w: lane configuration size", ErrInvalid)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var cfg Config
	if err := d.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%w: cannot decode lane configuration: %v", ErrInvalid, err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("%w: trailing lane configuration", ErrInvalid)
	}
	cfg.Defaults()
	return cfg, cfg.Validate()
}
