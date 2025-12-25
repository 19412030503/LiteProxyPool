package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"lite-proxy/logic"
)

type Duration struct {
	d   time.Duration
	set bool
}

func DurationValue(d time.Duration) Duration {
	return Duration{d: d, set: true}
}

func (d Duration) Duration() time.Duration { return d.d }

func (d Duration) IsSet() bool { return d.set }

func (d *Duration) UnmarshalJSON(b []byte) error {
	d.set = true
	d.d = 0

	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			d.d = 0
			return nil
		}
		dd, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		d.d = dd
		return nil
	}

	var seconds float64
	if err := json.Unmarshal(b, &seconds); err != nil {
		return err
	}
	d.d = time.Duration(seconds * float64(time.Second))
	return nil
}

type Config struct {
	SOCKSListen  string        `json:"socks_listen"`
	WebListen    string        `json:"web_listen"`
	RefreshEvery Duration      `json:"refresh_every"`
	RotateEvery  Duration      `json:"rotate_every"`
	DialTimeout  Duration      `json:"dial_timeout"`
	Sources      *logic.Sources `json:"sources"`
	Proxies      []string      `json:"proxies"`
	Validation   logic.ValidationConfig `json:"validation"`
}

func LoadConfig(path string) (Config, error) {
	if path == "" {
		return Config{}, errors.New("empty config path")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) ApplyDefaults() {
	if c.SOCKSListen == "" {
		c.SOCKSListen = "127.0.0.1:1080"
	}
	if c.WebListen == "" {
		c.WebListen = "127.0.0.1:8088"
	}
	if !c.RefreshEvery.IsSet() {
		c.RefreshEvery = DurationValue(30 * time.Minute)
	}
	if !c.RotateEvery.IsSet() {
		c.RotateEvery = DurationValue(30 * time.Second)
	}
	if !c.DialTimeout.IsSet() {
		c.DialTimeout = DurationValue(15 * time.Second)
	}
	if c.Sources == nil {
		ds := logic.DefaultSources()
		c.Sources = &ds
	}
	// Keep defaults in sync with logic.ValidationConfig.
	c.Validation.ApplyDefaults()
}

func (c *Config) Validate() error {
	if c.SOCKSListen == "" {
		return fmt.Errorf("socks_listen is empty")
	}
	if c.WebListen == "" {
		return fmt.Errorf("web_listen is empty")
	}
	if c.Sources == nil {
		return fmt.Errorf("sources is nil")
	}
	for i, s := range *c.Sources {
		if s.URL == "" {
			return fmt.Errorf("sources[%d].url is empty", i)
		}
		if err := s.Validate(); err != nil {
			return fmt.Errorf("sources[%d]: %w", i, err)
		}
	}
	return nil
}
