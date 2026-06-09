# OM1 Telemetry Recorder

A Go application that synchronously records multi-modal sensor data:
- **Video** from RTSP streams (via ffmpeg) — multiple cameras
- **Audio** from RTSP streams (via ffmpeg)
- **Lidar** point clouds from Zenoh topics
- **Depth** frames from Zenoh topics (raw, lossless)
- **Odometry** messages from Zenoh topics

All streams are timestamped and organized into session directories for easy alignment and analysis.

## Prerequisites

- **Go 1.25** or later
- **ffmpeg** installed and available in PATH
- **zenoh-c library** (automatically downloaded via `make download-zenohc`)

## Configuration

Configure via environment variables:

- `ENABLE_COLLECTION` - Enable/disable data collection (default: `true`; set to `false`, `0`, or `no` to disable)
- `TOP_CAMERA_RTSP_URL` - Top camera stream URL (default: `rtsp://localhost:8554/top_camera_raw`)
- `FRONT_CAMERA_RTSP_URL` - Front camera stream URL (default: `rtsp://localhost:8554/front_camera`)
- `DOWN_CAMERA_RTSP_URL` - Down camera stream URL (default: `rtsp://localhost:8554/down_camera`)
- `AUDIO_RTSP_URL` - Audio stream URL (default: `rtsp://localhost:8554/audio`)
- `LIDAR_ZENOH_ENDPOINT` - Zenoh endpoint for lidar (default: `tcp/127.0.0.1:7447`)
- `LIDAR_ZENOH_TOPIC` - Zenoh topic for lidar data (default: `scan`)
- `DEPTH_ZENOH_ENDPOINT` - Zenoh endpoint for depth (default: `tcp/127.0.0.1:7447`)
- `DEPTH_ZENOH_TOPIC` - Zenoh topic for depth frames (default: `camera/realsense2_camera_node/depth/image_rect_raw`)
- `ODOM_ZENOH_ENDPOINT` - Zenoh endpoint for odometry (default: `tcp/127.0.0.1:7447`)
- `ODOM_ZENOH_TOPIC` - Zenoh topic for odometry data (default: `odom`)
- `RECORDINGS_DIR` - Base directory for recordings (default: `recordings`)

## Building

Download the zenoh-c library and build the binary:

```bash
make download-zenohc
make build
```

The binary will be created at `bin/om1-telemetry`.

## Running

```bash
./bin/om1-telemetry
```

Or with custom settings:

```bash
ENABLE_COLLECTION=true \
TOP_CAMERA_RTSP_URL="rtsp://camera.local/top_camera_raw" \
FRONT_CAMERA_RTSP_URL="rtsp://camera.local/front_camera" \
DOWN_CAMERA_RTSP_URL="rtsp://camera.local/down_camera" \
AUDIO_RTSP_URL="rtsp://camera.local/audio" \
LIDAR_ZENOH_ENDPOINT="tcp/192.168.1.10:7447" \
LIDAR_ZENOH_TOPIC="scan" \
DEPTH_ZENOH_ENDPOINT="tcp/192.168.1.10:7447" \
DEPTH_ZENOH_TOPIC="camera/realsense2_camera_node/depth/image_rect_raw" \
ODOM_ZENOH_ENDPOINT="tcp/192.168.1.10:7447" \
ODOM_ZENOH_TOPIC="odom" \
RECORDINGS_DIR="/path/to/recordings" \
./bin/om1-telemetry
```

## Session Output

Each recording session creates a timestamped directory structure:

```
recordings/
└── 2026-05-15/
    └── 2026-05-15_14-30-00/
        ├── meta.json                  # Session metadata
        ├── top_camera.mp4             # Top camera recording
        ├── front_camera.mp4           # Front camera recording
        ├── down_camera.mp4            # Down camera recording
        ├── audio.ogg                  # Audio recording
        ├── lidar_scans.bin            # Raw lidar point cloud data
        ├── lidar_timestamps.csv       # Timestamps: unix_ns,seq,byte_offset
        ├── depth_frames.bin           # Raw depth frames (serialized ROS Image, 16UC1)
        ├── depth_timestamps.csv       # Timestamps: unix_ns,seq,byte_offset,byte_length
        ├── odom_frames.bin            # Raw odometry messages
        └── odom_timestamps.csv        # Timestamps: unix_ns,seq,byte_offset
```

Each row in `depth_timestamps.csv` slices one frame out of `depth_frames.bin`
using `byte_offset` and `byte_length`. The frames are the raw serialized
`sensor_msgs/Image` payloads — stored losslessly so the 16-bit depth values are
preserved exactly — and are decoded/aligned offline.

## Testing

Run the test suite:

```bash
make test
```

Run tests for a specific package:

```bash
make test
```

## Development

- **Linting**: `make lint` (requires golangci-lint)
- **Tidy dependencies**: `make tidy`
