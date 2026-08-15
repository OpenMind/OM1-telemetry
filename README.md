# OM1 Telemetry Recorder

A Go application that synchronously records multi-modal sensor data:
- **Video** from RTSP streams (via ffmpeg) — multiple cameras
- **Audio** from RTSP streams (via ffmpeg)
- **Video features** (capture-time-stamped, video-derived events) from the OM1 video-processor's feature log
- **Lidar** scans from a CycloneDDS topic
- **Point clouds** (Livox MID360) from a CycloneDDS topic (zstd-compressed, lossless)
- **Depth** frames from a CycloneDDS topic (RVL-compressed, lossless)
- **Odometry** messages from a CycloneDDS topic
- **Lowstate** (IMU/joints/battery/etc.) from a CycloneDDS topic

All streams are timestamped and organized into session directories for easy alignment and analysis.

## Prerequisites

- **Go 1.25** or later
- **ffmpeg** installed and available in PATH
- **CycloneDDS** (`libddsc` + `idlc`) installed and discoverable via `pkg-config` — see "Build prerequisites" below

### Build prerequisites (CycloneDDS)

Sensor/state topics (lidar, point cloud, depth, odometry, lowstate) are read by
subscribing directly to the robot's CycloneDDS domain, via a first-party cgo
wrapper (`internal/ddscore`) — there's no zenoh-bridge-dds hop anymore. This
means building or testing this recorder requires CycloneDDS's C library and
IDL compiler on the build host:

```bash
# Debian/Ubuntu
apt install libcyclonedds-dev cyclonedds-tools

# macOS (Homebrew)
brew install cyclonedds

# or build from source: https://github.com/eclipse-cyclonedds/cyclonedds
```

`make build` / `make test` / `make run` all depend on `make idl-gen`, which
runs `idlc` over `idl/*.idl` to produce the type-support C sources each
stream's `dds_reader.go` `#include`s (generated into `internal/ddsgen/`,
gitignored — regenerate after editing any `.idl` file).

**Message schemas**: `idl/*.idl` mirror the exact field layout of the ROS 2 /
Unitree messages published on the robot (`sensor_msgs/LaserScan`,
`sensor_msgs/PointCloud2`, `sensor_msgs/Image`, `nav_msgs/Odometry`,
`unitree_go/msg/LowState`, `unitree_hg/msg/LowState`), authored from
[unitreerobotics/unitree_ros2](https://github.com/unitreerobotics/unitree_ros2)
and [ros2/common_interfaces](https://github.com/ros2/common_interfaces). The
generated C type/topic-descriptor symbol names each `dds_reader.go`
references (e.g. `C.unitree_go_msg_dds__LowState_`,
`C.unitree_go_msg_dds__LowState__desc`) follow idlc's standard flattened
module-naming convention but have **not been confirmed against a real idlc
run** (idlc isn't available in every dev environment) — the first
`make build` on a host with CycloneDDS installed is the actual verification;
if idlc emits different symbol names, fix the `C.*` references in the
corresponding `dds_reader.go` to match.

## Configuration

### Robot profile

Set `ROBOT_TYPE` to select a built-in recording profile. The profile decides
which sensors are recorded and the robot-specific DDS topic defaults, and
(for lowstate) which message schema to subscribe with — `unitree_go/msg/
LowState` for Go2 vs `unitree_hg/msg/LowState` for G1; it is defined in code
(`config/profile.go`), so no profile files need to ship with the binary.

- `ROBOT_TYPE=go2` — Go2 quadruped: 2D `/scan` lidar enabled, 3D point cloud disabled.
- `ROBOT_TYPE=g1` — G1 humanoid: 3D point cloud (Livox Mid-360) and 2D `/scan` lidar both enabled.

If `ROBOT_TYPE` is unset or unrecognized, the recorder logs a warning and falls
back to the default profile (`go2`). The convenience env files set this for you:

```bash
source env.go2   # ROBOT_TYPE=go2 + deployment overrides
source env.g1    # ROBOT_TYPE=g1  + deployment overrides
./bin/om1-telemetry
```

### Environment variables

Any of the following override the selected profile / defaults:

- `ROBOT_TYPE` - Robot profile to load: `go2` or `g1` (default: `go2`)
- `ENABLE_LIDAR` - Record the 2D `/scan` lidar (default: from profile)
- `ENABLE_POINTCLOUD` - Record the 3D point cloud (default: from profile)
- `ENABLE_COLLECTION` - Enable/disable data collection (default: `true`; set to `false`, `0`, or `no` to disable)
- `TOP_CAMERA_RTSP_URL` - Top camera stream URL (default: `rtsp://localhost:8556/raw` — the video-processor's clean camera view, gst-direct)
- `FRONT_CAMERA_RTSP_URL` - Front camera stream URL (default: `rtsp://localhost:8554/front_camera`)
- `DOWN_CAMERA_RTSP_URL` - Down camera stream URL (default: `rtsp://localhost:8554/down_camera`)
- `AUDIO_RTSP_URL` - Audio stream URL (default: `rtsp://localhost:8555/live` — audio track of the video-processor's muxed session stream, gst-direct)
- `VIDEO_FEATURES_LOG` - Path to the video-processor's feature-log JSONL on a shared volume (default: empty — ingestion disabled)
- `LIDAR_DDS_DOMAIN` - CycloneDDS domain ID for lidar (default: `0`)
- `LIDAR_DDS_TOPIC` - DDS topic for lidar data (default: `scan`)
- `POINTCLOUD_DDS_DOMAIN` - CycloneDDS domain ID for point cloud (default: `0`)
- `POINTCLOUD_DDS_TOPIC` - DDS topic for point cloud data (default: `rt/utlidar/cloud_livox_mid360`)
- `DEPTH_DDS_DOMAIN` - CycloneDDS domain ID for depth (default: `0`)
- `DEPTH_DDS_TOPIC` - DDS topic for depth frames (default: `camera/realsense2_camera_node/depth/image_rect_raw`)
- `ODOM_DDS_DOMAIN` - CycloneDDS domain ID for odometry (default: `0`)
- `ODOM_DDS_TOPIC` - DDS topic for odometry data (default: `odom`)
- `LOWSTATE_DDS_DOMAIN` - CycloneDDS domain ID for lowstate (default: `0`)
- `LOWSTATE_DDS_TOPIC` - DDS topic for lowstate data (default: `rt/lowstate`)
- `RECORDINGS_DIR` - Base directory for recordings (default: `recordings`)

CycloneDDS domain IDs (not endpoints/addresses) are how independent DDS
"networks" on the same host/subnet are separated — leave at the default `0`
unless the robot's network config uses a non-default domain. To reach the
robot on a specific network interface, or a non-default domain, set
`CYCLONEDDS_URI` per [CycloneDDS's config
documentation](https://cyclonedds.io/docs/cyclonedds/latest/config/) rather
than anything in this table — it's read directly by libddsc, not this
recorder.

## Building

Install CycloneDDS (see "Build prerequisites" above), then:

```bash
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
TOP_CAMERA_RTSP_URL="rtsp://localhost:8556/raw" \
FRONT_CAMERA_RTSP_URL="rtsp://camera.local/front_camera" \
DOWN_CAMERA_RTSP_URL="rtsp://camera.local/down_camera" \
AUDIO_RTSP_URL="rtsp://localhost:8555/live" \
VIDEO_FEATURES_LOG="/shared/video-processor/features.jsonl" \
LIDAR_DDS_TOPIC="scan" \
POINTCLOUD_DDS_TOPIC="rt/utlidar/cloud_livox_mid360" \
DEPTH_DDS_TOPIC="camera/realsense2_camera_node/depth/image_rect_raw" \
ODOM_DDS_TOPIC="odom" \
LOWSTATE_DDS_TOPIC="rt/lowstate" \
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
        ├── video_features.jsonl       # Verbatim video-processor feature events (if VIDEO_FEATURES_LOG set)
        ├── video_features_timestamps.csv  # unix_ns,t_capture_ns,type,seq (capture time)
        ├── video_features_timebase.json   # monotonic<->UTC mapping from the log header
        ├── lidar_scans.bin            # Raw lidar point cloud data
        ├── lidar_timestamps.csv       # Timestamps: unix_ns,seq,byte_offset
        ├── pointcloud_frames.bin      # zstd-compressed Livox MID360 PointCloud2 frames
        ├── pointcloud_timestamps.csv  # unix_ns,seq,byte_offset,byte_length,method
        ├── depth_frames.bin           # RVL-compressed depth frames (lossless)
        ├── depth_timestamps.csv       # unix_ns,seq,byte_offset,byte_length,method,width,height,encoding
        ├── odom_frames.bin            # Raw odometry messages
        └── odom_timestamps.csv        # Timestamps: unix_ns,seq,byte_offset
```

### Depth frames (RVL)

Each row in `depth_timestamps.csv` slices one frame out of `depth_frames.bin`
using `byte_offset` and `byte_length`. Depth is compressed with **RVL**
(Wilson, *Fast Lossless Depth Image Compression*, ISS 2017) — a lossless codec
designed for 16-bit depth maps, so depth values are preserved exactly while
typically using far less space than the raw frames.

The extra columns describe each frame so it can be reconstructed offline:

- `method` — `rvl` for RVL-compressed frames, or `raw` for the fallback (see below).
- `width`, `height` — frame dimensions; the decoded frame has `width*height` 16-bit pixels.
- `encoding` — the source ROS image encoding (e.g. `16UC1`).

To decode a frame: read `byte_length` bytes at `byte_offset`, then RVL-decode
into `width*height` little-endian `uint16` pixels.

**Fallback:** if a payload can't be parsed as a 16-bit depth image, it is stored
verbatim with `method=raw` (the original serialized `sensor_msgs/Image`), so no
data is ever lost — those rows are decoded by parsing the ROS message directly.

### Point cloud frames (zstd)

Each row in `pointcloud_timestamps.csv` slices one frame out of
`pointcloud_frames.bin` using `byte_offset` and `byte_length`. Each
`sensor_msgs/PointCloud2` payload is compressed independently with **zstd**
(lossless), so frames stay randomly accessible and decode exactly.

- `method` — `zstd` for compressed frames, or `raw` for the fallback.
- `byte_length` — the number of bytes the frame occupies in the data file.

To decode a frame: read `byte_length` bytes at `byte_offset`, then zstd-decode
(for `method=zstd`) to recover the original serialized `sensor_msgs/PointCloud2`.

**Fallback:** if compression wouldn't shrink a payload, it is stored verbatim
with `method=raw`, so no data is ever lost.

### Video features & precision time

The recorder captures data on a common **UTC-nanosecond** clock so that
`align_recording.py` can nearest-neighbour join every modality. The DDS
streams (lidar, point cloud, depth, odometry, lowstate) carry a *source*
timestamp — precise (CycloneDDS's `dds_sample_info_t.source_timestamp`,
nanoseconds since the Unix epoch). The camera/audio streams, however, are
anchored to the ffmpeg **record-start wall clock** plus each segment's PTS
offset, so they are skewed from true capture by the RTSP/pipeline latency at
connect time.

To recover a *capture-accurate* video timeline, point `VIDEO_FEATURES_LOG` at
the OM1 video-processor's feature-log JSONL. The video-processor stamps every
feature event (VVAD/speaking today; pose, recognition, etc. later) at
frame-capture on its shared `CLOCK_MONOTONIC`, and writes a monotonic→UTC
mapping in a header record. This recorder tails that file and emits
`video_features_timestamps.csv` keyed to **true capture UTC time** — directly
comparable to the source-stamped DDS streams, without relying on fragile A/V
stream synchronisation.

Deployment requirement: the feature log is written *inside* the video-processor
container, so its file must live on a volume shared with this recorder, and
`VIDEO_FEATURES_LOG` here must point at the same path the video-processor's
`--feature-log` / `OM_FEATURE_LOG` writes. On the first open, the recorder skips
the pre-session backlog and records only events from session start; it follows
rotation of the source file.

If you need frame-accurate timing for the recorded **video pixels** (not just
the feature events) — e.g. to align a specific mp4 frame to a lidar scan within
a few ms — the camera recorder would need to read per-frame capture time from
RTP/RTCP sender reports (e.g. via a `gortsplib`-based client) instead of the
ffmpeg record-start anchor. That is a larger change and is intentionally not
done here; the feature-log timeline covers the common case.

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
