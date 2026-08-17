package config

import (
	"log/slog"
	"os"
	"sort"
	"strings"
)

type RobotType string

const (
	RobotGo2 RobotType = "go2"
	RobotG1  RobotType = "g1"
)

const DefaultRobotType = RobotGo2

type Profile struct {
	EnableLidar      bool
	EnablePointCloud bool

	LidarTopic      string
	PointCloudTopic string
	OdomTopic       string
	LowstateTopic   string
	DepthTopic      string
}

var profiles = map[RobotType]Profile{
	RobotGo2: {
		EnableLidar:      true,
		EnablePointCloud: false,
		LidarTopic:       "rt/scan",
		PointCloudTopic:  "rt/utlidar/cloud_deskewed",
		OdomTopic:        "rt/odom",
		LowstateTopic:    "rt/lowstate",
		DepthTopic:       "rt/camera/realsense2_camera_node/depth/image_rect_raw",
	},
	RobotG1: {
		EnableLidar:      true,
		EnablePointCloud: true,
		LidarTopic:       "rt/scan",
		PointCloudTopic:  "rt/utlidar/cloud_livox_mid360",
		OdomTopic:        "rt/odom",
		LowstateTopic:    "rt/lowstate",
		DepthTopic:       "rt/camera/realsense2_camera_node/depth/image_rect_raw",
	},
}

func ResolveProfile() (RobotType, Profile) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("ROBOT_TYPE")))
	if raw == "" {
		slog.Warn("ROBOT_TYPE unset; using default profile",
			"default", DefaultRobotType, "supported", supportedRobotTypes())
		return DefaultRobotType, profiles[DefaultRobotType]
	}

	rt := RobotType(raw)
	profile, ok := profiles[rt]
	if !ok {
		slog.Warn("unrecognized ROBOT_TYPE; using default profile",
			"got", raw, "default", DefaultRobotType, "supported", supportedRobotTypes())
		return DefaultRobotType, profiles[DefaultRobotType]
	}
	return rt, profile
}

func supportedRobotTypes() []string {
	names := make([]string, 0, len(profiles))
	for rt := range profiles {
		names = append(names, string(rt))
	}
	sort.Strings(names)
	return names
}
