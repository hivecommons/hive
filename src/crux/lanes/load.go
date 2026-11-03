package lanes

import (
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
	d := json.NewDecoder(io.LimitReader(f, maxConfigBytes+1))
	d.DisallowUnknownFields()
	var cfg Config
	if err := d.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%w: cannot decode lane configuration", ErrInvalid)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("%w: trailing lane configuration", ErrInvalid)
	}
	cfg.Defaults()
	return cfg, cfg.Validate()
}
