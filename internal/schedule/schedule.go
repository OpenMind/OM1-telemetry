// Package schedule loads an optional daily recording/uploading schedule
// and reports whether each axis should be active at a given moment.
package schedule

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is a daily schedule for the recording/upload axes. A nil Window means that axis is always on.
type Config struct {
	Recording *Window `yaml:"recording"`
	Upload    *Window `yaml:"upload"`
}

// Window is a daily recurring time-of-day range. End earlier than Start means the window spans midnight.
type Window struct {
	Start dayMinute `yaml:"start"`
	End   dayMinute `yaml:"end"`
}

// Active reports whether t's time-of-day falls within the window.
func (w *Window) Active(t time.Time) bool {
	if w == nil {
		return true
	}
	now := dayMinute(t.Hour()*60 + t.Minute())
	if w.Start == w.End {
		return true
	}
	if w.Start < w.End {
		return now >= w.Start && now < w.End
	}
	return now >= w.Start || now < w.End
}

// dayMinute is minutes since midnight, parsed from a "HH:MM" string.
type dayMinute int

func (d *dayMinute) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return fmt.Errorf("invalid time %q, want HH:MM (24h): %w", s, err)
	}
	*d = dayMinute(t.Hour()*60 + t.Minute())
	return nil
}

// Load reads and parses a schedule file; an empty path returns (nil, nil).
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &cfg, nil
}
