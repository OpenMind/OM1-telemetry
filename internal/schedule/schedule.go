// Package schedule loads an optional daily recording/uploading schedule
// and reports, for a given moment, whether each axis should be active. It
// is deliberately ignorant of control.State or the recorder's event loop
// -- see cmd/main's runSchedule for how a Config drives those -- so it
// stays trivial to test and easy to point at from something other than
// cmd/main if a different scheme is ever wanted.
package schedule

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is a daily schedule for the two control-API axes. A nil Window on
// either field means that axis has no schedule and is always on -- the
// same behavior as if no schedule file were configured at all. This lets
// a deployment schedule just one axis (say, uploads only) and leave the
// other continuous.
type Config struct {
	Recording *Window `yaml:"recording"`
	Upload    *Window `yaml:"upload"`
}

// Window is a daily recurring time-of-day range, e.g. 09:00-17:00, in
// whatever timezone the process's clock is set to. End earlier than Start
// means the window spans midnight -- e.g. 17:00-09:00 covers the overnight
// hours -- rather than being an error.
type Window struct {
	Start dayMinute `yaml:"start"`
	End   dayMinute `yaml:"end"`
}

// Active reports whether t's time-of-day falls within the window. A nil
// *Window (the zero value for an omitted YAML section) is always active,
// so callers don't need to special-case "no schedule for this axis"
// separately from "schedule says on right now".
func (w *Window) Active(t time.Time) bool {
	if w == nil {
		return true
	}
	now := dayMinute(t.Hour()*60 + t.Minute())
	if w.Start == w.End {
		// A zero-length window (including the zero value, 00:00-00:00, from
		// an accidentally-empty `recording: {}`) is degenerate. Treating it
		// as always-on rather than always-off matches this package's overall
		// bias: a misconfigured schedule should fail open into recording
		// everything, not silently stop.
		return true
	}
	if w.Start < w.End {
		return now >= w.Start && now < w.End
	}
	return now >= w.Start || now < w.End // spans midnight
}

// dayMinute is minutes since midnight, 0-1439, parsed from a "HH:MM"
// string in the YAML source.
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

// Load reads and parses a schedule file. An empty path is not an error --
// it returns (nil, nil), the caller's signal (see cmd/main's setupSchedule)
// to skip scheduling entirely and keep the recorder's default always-on
// behavior, matching how the feature behaves when SCHEDULE_FILE is unset.
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
