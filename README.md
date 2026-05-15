# OM1 Telemetry Recorder

A Go application that synchronously records multi-modal sensor data:
- **Video** from RTSP streams (via ffmpeg)
- **Audio** from RTSP streams (via ffmpeg)
- **Lidar** point clouds from Zenoh topics

All streams are timestamped and organized into session directories for easy alignment and analysis.

## Prerequisites

- **Go 1.25** or later
- **ffmpeg** installed and available in PATH
- **zenoh-c library** (automatically downloaded via `make download-zenohc`)

## Configuration

Configure via environment variables:

- `ENABLE_COLLECTION` - Enable/disable data collection (default: `true`; set to `false`, `0`, or `no` to disable)
- `VIDEO_RTSP_URL` - Video stream URL (default: `rtsp://localhost:8554/top_camera_raw`)
- `AUDIO_RTSP_URL` - Audio stream URL (default: `rtsp://localhost:8554/audio`)
- `LIDAR_ZENOH_ENDPOINT` - Zenoh endpoint for lidar (default: `tcp/127.0.0.1:7447`)
- `LIDAR_ZENOH_TOPIC` - Zenoh topic for lidar data (default: `scan`)
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
VIDEO_RTSP_URL="rtsp://camera.local/stream" \
AUDIO_RTSP_URL="rtsp://camera.local/audio" \
LIDAR_ZENOH_ENDPOINT="tcp/192.168.1.10:7447" \
LIDAR_ZENOH_TOPIC="scan" \
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
        ├── video.mp4                  # Video recording
        ├── audio.ogg                  # Audio recording
        ├── lidar_scans.bin           # Raw lidar point cloud data
        └── lidar_timestamps.csv       # Timestamps: unix_ns,seq,byte_offset
```

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
