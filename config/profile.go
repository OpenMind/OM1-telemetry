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
	// What this class of robot records by default. An individual unit that
	// deviates -- a G1 shipped without a RealSense, say -- overrides the one
	// switch it needs with ENABLE_<TOPIC>, so ROBOT_TYPE stays the only
	// variable a normal deployment sets.
	EnableLidar      bool
	EnablePointCloud bool
	EnableDepth      bool
	EnableOdom       bool
	EnableLowstate   bool

	LidarTopic      string
	PointCloudTopic string
	OdomTopic       string
	LowstateTopic   string
	DepthTopic      string

	// Cameras this robot records. A G1 and a Go2 do not have the same ones, so
	// the list belongs to the profile rather than being global: recording a
	// camera the robot does not have costs a reconnect every two seconds and a
	// 0-byte mp4 each time.
	Cameras []CameraSpec
}

// CameraSpec is one RTSP source recorded to <Name>.mp4.
type CameraSpec struct {
	Name string
	URL  string
	// Env optionally names the variable that overrides URL. Held here rather
	// than derived from Name so the historical variable names survive a camera
	// being renamed.
	Env string
}

var profiles = map[RobotType]Profile{
	RobotGo2: {
		EnableLidar:      true,
		EnablePointCloud: false,
		EnableDepth:      true,
		EnableOdom:       true,
		EnableLowstate:   true,
		LidarTopic:       "rt/scan",
		PointCloudTopic:  "rt/utlidar/cloud_deskewed",
		OdomTopic:        "rt/odom",
		LowstateTopic:    "rt/lowstate",
		DepthTopic:       "rt/camera/realsense2_camera_node/depth/image_rect_raw",
		Cameras: []CameraSpec{
			// top_camera is the video-processor's clean (pre-CV) view on
			// gst-direct :8556. front/down are served by other sources on the
			// robot, on their existing mountpoints.
			{Name: "top_camera", URL: "rtsp://localhost:8556/raw", Env: "TOP_CAMERA_RTSP_URL"},
			{Name: "front_camera", URL: "rtsp://localhost:8554/front_camera", Env: "FRONT_CAMERA_RTSP_URL"},
			{Name: "down_camera", URL: "rtsp://localhost:8554/down_camera", Env: "DOWN_CAMERA_RTSP_URL"},
		},
	},
	RobotG1: {
		EnableLidar:      true,
		EnablePointCloud: true,
		// A D435i is fitted to this fleet's G1: measured 14.9 Hz at 480x270
		// 16UC1. On a G1 without one, realsense2_camera_node still starts and
		// logs "No RealSense devices were found!", so the topic exists but
		// never carries a message -- recording it writes a 0-byte
		// depth_frames.bin and leaves the heartbeat broken. Those units set
		// ENABLE_DEPTH=false.
		EnableDepth:     true,
		EnableOdom:      true,
		EnableLowstate:  true,
		LidarTopic:      "rt/scan",
		PointCloudTopic: "rt/utlidar/cloud_livox_mid360",
		OdomTopic:       "rt/odom",
		LowstateTopic:   "rt/lowstate",
		DepthTopic:      "rt/camera/realsense2_camera_node/depth/image_rect_raw",
		Cameras: []CameraSpec{
			// The video-processor's two pre-CV streams, gst-direct. Verified on
			// a live G1: both h264 1280x720 at 30 fps.
			{Name: "front_camera_raw", URL: "rtsp://localhost:8556/raw", Env: "FRONT_CAMERA_RAW_RTSP_URL"},
			{Name: "rear_camera_raw", URL: "rtsp://localhost:8558/raw", Env: "REAR_CAMERA_RAW_RTSP_URL"},
			// The RealSense's RGB view, which points down. om1_sensor's
			// d435_camera_stream publishes it to mediamtx as H.264 already (its
			// get_rtsp_camera_name returns "down_camera"), about 0.18 GB/h. The
			// same image is on DDS as raw rgb8 -- 16.5 GB/h -- so record the
			// stream the robot has already encoded. 404s if no RealSense is
			// fitted, since the node then has nothing to publish.
			{Name: "down_camera", URL: "rtsp://localhost:8554/down_camera", Env: "DOWN_CAMERA_RTSP_URL"},
		},
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
