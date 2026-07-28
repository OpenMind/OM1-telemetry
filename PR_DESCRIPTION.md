# Repoint to the video-processor topology + ingest capture-time feature log

## Why

The OM1 video-processor moved to a single GStreamer capture that serves
`/live` (A+V, gst `:8555`) and `/raw` (clean video, gst `:8556`), fronted by a
mediamtx hub on `:8554`. The telemetry recorder's video/audio defaults still
pointed at the **removed** legacy mediamtx mountpoints, and its recorded
video/audio is only anchored to ffmpeg record-start wall clock — skewed from
true capture by RTSP/pipeline latency, so it doesn't align cleanly with the
source-stamped Zenoh sensors.

## What changed

**1. Endpoint repoint (connect to the right places).** Telemetry runs on the
Thor and is a precision recorder, so it consumes the **gst-direct** endpoints
(lowest latency, closest to capture):

- `top_camera`: `:8554/top_camera_raw` → `rtsp://localhost:8556/raw`
- audio: `:8554/audio` → `rtsp://localhost:8555/live` (audio track of the muxed session)
- `front_camera` / `down_camera`: **unchanged** (served by other sources on the robot)
- `script/precheck_go2.sh` health-checks updated to match

All endpoints stay env-overridable (e.g. point at mediamtx `:8554` for an
off-host recorder). The Zenoh streams (lidar/pointcloud/depth/odom/lowstate)
are untouched — they already carry precise source timestamps.

**2. Precision time — ingest the feature log (`internal/features`).** New
recorder tails the video-processor's feature-log JSONL (VVAD/pose events stamped
at frame-capture on `CLOCK_MONOTONIC`, with a monotonic→UTC header) and writes:

- `video_features.jsonl` — verbatim copy of ingested records
- `video_features_timestamps.csv` — `unix_ns,t_capture_ns,type,seq` on **true capture UTC time**
- `video_features_timebase.json` — the monotonic↔UTC mapping from the header

This gives a capture-accurate, video-derived timeline that aligns precisely with
the Zenoh sensors on the common UTC-ns clock, without depending on fragile A/V
stream sync. The tailer skips pre-session backlog, follows source rotation, and
reports liveness via the heartbeat monitor. Added as a `video_features` stream
in `script/align_recording.py`.

Gated on `VIDEO_FEATURES_LOG` (empty = disabled). No behaviour change when unset.

## Files

- `config/config.go` — repointed audio/top_camera defaults; `FeaturesConfig` + `VIDEO_FEATURES_LOG`
- `cmd/main/main.go` — construct/start/stop the feature recorder + heartbeat registration
- `internal/features/stream.go` — new tailer/ingestor
- `script/align_recording.py` — new `video_features` stream
- `script/precheck_go2.sh` — updated RTSP health-checks
- `README.md` — env table, session-output tree, precision-time section

## Deployment notes

- Telemetry must run on the Thor host (localhost defaults) to reach the
  gst-direct ports.
- The feature log is written **inside** the video-processor container, so its
  file must live on a volume shared with this recorder, and `VIDEO_FEATURES_LOG`
  must point at the same path the video-processor's `--feature-log` /
  `OM_FEATURE_LOG` writes.

## Testing

Go is not available in the authoring environment — **CI must run
`go build ./...`, `go vet ./...`, and `go test ./...`**. `align_recording.py`
parses clean. Manual review covered imports, references, and the module path.

## Follow-up (not in this PR)

Frame-accurate timing for the recorded video **pixels** (aligning a specific
mp4 frame to a lidar scan within a few ms) would require reading per-frame
capture time from RTP/RTCP sender reports (e.g. a `gortsplib`-based client)
instead of the ffmpeg record-start anchor. The feature-log timeline covers the
common case; this is deferred.
