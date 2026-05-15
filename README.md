# RTSP Recorder

A simple Go application that records video and audio from RTSP streams to local files.

## Prerequisites

- Go 1.25 or later
- ffmpeg installed and available in PATH

## Configuration

Configure via environment variables:

- `VIDEO_RTSP_URL` - Video stream URL (default: `rtsp://localhost:8554/live`)
- `AUDIO_RTSP_URL` - Audio stream URL (default: `rtsp://localhost:8554/audio`)
- `RECORDINGS_DIR` - Base directory for recordings (default: `recordings`)

## Build

```bash
go build ./cmd/main
```

## Run

```bash
./main
```

Or with custom settings:

```bash
VIDEO_RTSP_URL="rtsp://camera.local/stream" \
AUDIO_RTSP_URL="rtsp://camera.local/audio" \
RECORDINGS_DIR="/path/to/recordings" \
./main
```
