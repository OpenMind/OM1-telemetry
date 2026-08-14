package config

import (
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// robots.yaml is embedded so the binary carries a working profile set with no
// volume mounted. ROBOT_PROFILES_FILE replaces it at runtime.
//
//go:embed robots.yaml
var embeddedProfiles []byte

type RobotType string

const (
	RobotGo2 RobotType = "go2"
	RobotG1  RobotType = "g1"
)

const DefaultRobotType = RobotGo2

// Topic names referenced by the recorder. A profile that omits one records
// nothing for it; a profile that sets enabled:false records nothing but keeps
// the key documented for whoever fits the hardware later.
const (
	TopicLidar      = "lidar"
	TopicPointCloud = "pointcloud"
	TopicOdom       = "odom"
	TopicLowstate   = "lowstate"
	TopicDepth      = "depth"
)

// CameraProfile is one RTSP source recorded to <name>.mp4 in the session dir.
type CameraProfile struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// Env optionally names an environment variable that overrides URL. Kept in
	// the file rather than derived from Name so the historical variable names
	// (TOP_CAMERA_RTSP_URL) survive a camera being renamed.
	Env string `yaml:"env"`
}

// AudioProfile is the RTSP source recorded to audio.ogg.
type AudioProfile struct {
	URL string `yaml:"url"`
	Env string `yaml:"env"`
}

// TopicProfile is one Zenoh topic recorded to <name>_frames.bin.
type TopicProfile struct {
	Enabled bool    `yaml:"enabled"`
	Key     string  `yaml:"key"`
	RateHz  float64 `yaml:"rate_hz"`
}

// Profile is everything one robot type records.
type Profile struct {
	Cameras []CameraProfile         `yaml:"cameras"`
	Audio   AudioProfile            `yaml:"audio"`
	Topics  map[string]TopicProfile `yaml:"topics"`
}

// Topic returns the profile entry for name and whether the profile declares it.
func (p Profile) Topic(name string) (TopicProfile, bool) {
	t, ok := p.Topics[name]
	return t, ok
}

type profileFile struct {
	Robots map[RobotType]Profile `yaml:"robots"`
}

// loadProfiles reads the profile set from ROBOT_PROFILES_FILE, falling back to
// the embedded copy. A malformed override falls back rather than exiting: a bad
// mount should not stop a robot from recording.
func loadProfiles() map[RobotType]Profile {
	if path := os.Getenv("ROBOT_PROFILES_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			slog.Error("cannot read ROBOT_PROFILES_FILE; using embedded profiles",
				"path", path, "err", err)
		} else if parsed, err := parseProfiles(raw); err != nil {
			slog.Error("cannot parse ROBOT_PROFILES_FILE; using embedded profiles",
				"path", path, "err", err)
		} else {
			slog.Info("loaded robot profiles from file", "path", path,
				"robots", robotNames(parsed))
			return parsed
		}
	}

	parsed, err := parseProfiles(embeddedProfiles)
	if err != nil {
		// Unreachable short of shipping a broken build: the embedded file is
		// parsed by TestEmbeddedProfiles_parse in CI.
		slog.Error("embedded robot profiles are unparseable", "err", err)
		return map[RobotType]Profile{}
	}
	return parsed
}

func parseProfiles(raw []byte) (map[RobotType]Profile, error) {
	var f profileFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if len(f.Robots) == 0 {
		return nil, fmt.Errorf("no robots defined")
	}
	for rt, p := range f.Robots {
		for i, cam := range p.Cameras {
			if cam.Name == "" {
				return nil, fmt.Errorf("robot %s: camera %d has no name", rt, i)
			}
			if cam.URL == "" {
				return nil, fmt.Errorf("robot %s: camera %q has no url", rt, cam.Name)
			}
		}
		for name, t := range p.Topics {
			if t.Key == "" {
				return nil, fmt.Errorf("robot %s: topic %q has no key", rt, name)
			}
		}
	}
	return f.Robots, nil
}

// ResolveProfile picks the profile for ROBOT_TYPE, falling back to the default
// robot type when it is unset or unrecognized.
func ResolveProfile() (RobotType, Profile) {
	profiles := loadProfiles()

	raw := strings.ToLower(strings.TrimSpace(os.Getenv("ROBOT_TYPE")))
	if raw == "" {
		slog.Warn("ROBOT_TYPE unset; using default profile",
			"default", DefaultRobotType, "supported", robotNames(profiles))
		return DefaultRobotType, profiles[DefaultRobotType]
	}

	rt := RobotType(raw)
	profile, ok := profiles[rt]
	if !ok {
		slog.Warn("unrecognized ROBOT_TYPE; using default profile",
			"got", raw, "default", DefaultRobotType, "supported", robotNames(profiles))
		return DefaultRobotType, profiles[DefaultRobotType]
	}
	return rt, profile
}

func robotNames(profiles map[RobotType]Profile) []string {
	names := make([]string, 0, len(profiles))
	for rt := range profiles {
		names = append(names, string(rt))
	}
	sort.Strings(names)
	return names
}
