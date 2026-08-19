# OM1 Telemetry Recorder

A Go application that synchronously records multi-modal sensor data:
- **Video** from RTSP streams (via ffmpeg) — multiple cameras; on the G1 both
  the front and rear pre-CV streams from the OM1 video-processor
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
- **CMake + a C compiler** (to build CycloneDDS — see "Build prerequisites" below)

### Build prerequisites (CycloneDDS)

Sensor/state topics (lidar, point cloud, depth, odometry, lowstate) are read by
subscribing directly to the robot's CycloneDDS domain, via a first-party cgo
wrapper (`internal/ddscore`) — there's no zenoh-bridge-dds hop anymore.

Rather than relying on distro packages (which vary release-to-release and
aren't available everywhere), CycloneDDS is built from source, the same way
for every developer, CI, and Docker: `make build` / `make test` / `make run`
all depend on `make install-cyclonedds`, which clones
[eclipse-cyclonedds/cyclonedds](https://github.com/eclipse-cyclonedds/cyclonedds)
(`releases/0.10.x`) and builds+installs it into `.cyclonedds/install`
(gitignored) if not already present there — a no-op on repeat runs. You need
`cmake` and a C compiler on PATH; nothing else to install manually.

`make build` / `make test` / `make run` also depend on `make idl-gen`, which
runs the freshly-built `idlc` over `idl/*.idl` to produce the type-support C
sources each stream's `dds_reader.go` `#include`s (generated into
`internal/ddsgen/`, gitignored — regenerate after editing any `.idl` file;
`make` does this automatically).

**Message schemas**: `idl/*.idl` mirror the exact field layout of the ROS 2 /
Unitree messages published on the robot (`sensor_msgs/LaserScan`,
`sensor_msgs/PointCloud2`, `sensor_msgs/Image`, `nav_msgs/Odometry`,
`unitree_go/msg/LowState`, `unitree_hg/msg/LowState`), authored from
[unitreerobotics/unitree_ros2](https://github.com/unitreerobotics/unitree_ros2)
and [ros2/common_interfaces](https://github.com/ros2/common_interfaces). The
generated C type/topic-descriptor symbol names each `dds_reader.go`
references (e.g. `C.unitree_go_msg_dds__LowState_`,
`C.unitree_go_msg_dds__LowState__desc`) have been confirmed against a real
`idlc` (CycloneDDS 0.10.5) run and a full `make build` + `make test` pass.

## Configuration

### Robot profile (`config/robots.yaml`)

Set `ROBOT_TYPE` to select a built-in recording profile. The profile decides
which sensors are recorded, the robot-specific DDS topic defaults, which
cameras exist, and (for lowstate) which message schema to subscribe with --
`unitree_go/msg/LowState` for Go2 vs `unitree_hg/msg/LowState` for G1. It is
defined in code (`config/profile.go`), so no profile files need to ship with
the binary.

| | `ROBOT_TYPE=g1` | `ROBOT_TYPE=go2` |
|---|---|---|
| cameras | `front_camera_raw` (:8556/raw), `rear_camera_raw` (:8558/raw), `down_camera` (:8554/down_camera) | `top_camera` (:8556/raw), `front_camera`, `down_camera` |
| lidar `scan` | ✅ | ✅ |
| point cloud | ✅ `rt/utlidar/cloud_livox_mid360` | — not published by this robot |
| odometry, lowstate | ✅ | ✅ |
| depth | ✅ D435i, 15 Hz (RVL, lossless) | ✅ |
| down camera | ✅ RealSense RGB via `down_camera` RTSP, 15 Hz | ✅ |

If `ROBOT_TYPE` is unset or unrecognized the recorder warns and falls back to
`go2`.

#### `depth` on a G1 without a RealSense

`realsense2_camera_node` starts inside `om1_sensor` on every G1 whether or not
a camera is attached, and logs `No RealSense devices were found!` when there is
none. The topic still exists and is subscribed successfully — it simply never carries a message, which writes a
0-byte `depth_frames.bin` every session and leaves the `depth` heartbeat
permanently broken.

The profile has it enabled because this fleet's G1 has a D435i fitted
(measured 14.9 Hz, 480x270 `16UC1`, RVL-compressed to ~34% of raw, lossless).
On a G1 without one, set `ENABLE_DEPTH=false`.

#### The G1's down camera is the RealSense RGB view

There is no third USB camera on a G1 -- only `OM1FRONTCAM` and `OM1REARCAM`.
The downward-facing view is the RealSense's colour image, and om1_sensor's
`d435_camera_stream` node already publishes it to mediamtx as H.264 (see its
`get_rtsp_camera_name`, which returns `"down_camera"`), so it is recorded like
any other camera.

The same image is also on DDS as
`rt/camera/realsense2_camera_node/color/image_raw`, but as raw rgb8: 305 KB a
frame, 16.5 GB an hour. Re-encoding that here costs about 5x the RTSP stream
(measured: JPEG q80 ~0.96 GB/h against 0.18 GB/h for the H.264 already being
produced) for a picture the robot has encoded anyway. Record the stream.

It 404s on a G1 with no RealSense fitted, since the node has nothing to publish.

#### Why `go2` has no `pointcloud` entry

It previously carried `rt/utlidar/cloud_deskewed` behind `EnablePointCloud:
false` — a topic the Go2 does not publish, disabled but still written down,
which read as "flip the switch and it records". The entry is gone. Setting
`ENABLE_POINTCLOUD=true` on a Go2 now logs that no topic is configured instead
of silently subscribing to nothing; supplying `POINTCLOUD_DDS_TOPIC` as well
enables it properly.

### Environment variables

Any of the following override the selected profile / defaults:

- `ROBOT_TYPE` - Robot profile to load: `go2` or `g1` (default: `go2`)
- `ENABLE_LIDAR` - Record the 2D `/scan` lidar (default: from profile)
- `ENABLE_POINTCLOUD` - Record the 3D point cloud (default: from profile)
- `ENABLE_COLLECTION` - Enable/disable data collection (default: `true`; set to `false`, `0`, or `no` to disable)
- `ROBOT_PROFILES_FILE` - Path to a `robots.yaml` replacing the embedded profiles (default: empty — use embedded)
- `ENABLE_DEPTH` / `ENABLE_ODOM` / `ENABLE_LOWSTATE` - Record these topics (default: from profile)
- `FRONT_CAMERA_RAW_RTSP_URL` - G1 front pre-CV stream (default: `rtsp://localhost:8556/raw`, gst-direct)
- `REAR_CAMERA_RAW_RTSP_URL` - G1 rear pre-CV stream (default: `rtsp://localhost:8558/raw`, gst-direct)
- `TOP_CAMERA_RTSP_URL` - Go2 top camera stream URL (default: `rtsp://localhost:8556/raw`)
- `FRONT_CAMERA_RTSP_URL` - Go2 front camera stream URL (default: `rtsp://localhost:8554/front_camera`)
- `DOWN_CAMERA_RTSP_URL` - Go2 down camera stream URL (default: `rtsp://localhost:8554/down_camera`)
- `AUDIO_RTSP_URL` - Audio stream URL (default: `rtsp://localhost:8555/live` — audio track of the video-processor's muxed session stream, gst-direct)
- `VIDEO_FEATURES_LOG` - Path to the video-processor's feature-log JSONL on a shared volume (default: empty — ingestion disabled)
- `LIDAR_DDS_DOMAIN` - CycloneDDS domain ID for lidar (default: `0`)
- `LIDAR_DDS_TOPIC` - DDS topic for lidar data (default: `rt/scan`)
- `POINTCLOUD_DDS_DOMAIN` - CycloneDDS domain ID for point cloud (default: `0`)
- `POINTCLOUD_DDS_TOPIC` - DDS topic for point cloud data (default: `rt/utlidar/cloud_livox_mid360`)
- `DEPTH_DDS_DOMAIN` - CycloneDDS domain ID for depth (default: `0`)
- `DEPTH_DDS_TOPIC` - DDS topic for depth frames (default: `rt/camera/realsense2_camera_node/depth/image_rect_raw`)
- `ODOM_DDS_DOMAIN` - CycloneDDS domain ID for odometry (default: `0`)
- `ODOM_DDS_TOPIC` - DDS topic for odometry data (default: `rt/odom`)
- `LOWSTATE_DDS_DOMAIN` - CycloneDDS domain ID for lowstate (default: `0`)
- `LOWSTATE_DDS_TOPIC` - DDS topic for lowstate data (default: `rt/lowstate`)
- `RECORDINGS_DIR` - Base directory for recordings (default: `recordings`)
- `SESSION_ROTATE_INTERVAL` - Close the current session and open a fresh one on this cadence, so each segment can be uploaded without waiting for the whole run to end (default: `5m`; `0` disables rotation — one session for the life of the process, as before this feature existed)
- `UPLOAD_ENABLED` - Upload each rotated-away session to the openmind-api (default: `true`; actual uploading additionally requires `OPENMIND_API_URL` and `OPENMIND_API_KEY` — see "Cloud upload" below)
- `OPENMIND_API_URL` - Base URL of the openmind-api, e.g. `https://<host>/api/core/v1` (default: empty — upload stays off, recording-only, until this is set)
- `OPENMIND_API_KEY` - API key sent as `Authorization: Bearer <key>` (default: empty)
- `DELETE_AFTER_UPLOAD` - Delete a session's local files once it has uploaded successfully (default: `false` — keep everything locally regardless of upload)
- `UPLOAD_MULTIPART_THRESHOLD_BYTES` - Files at or above this size go through S3 multipart upload instead of a single presigned POST (default: `104857600`, 100 MiB)
- `UPLOAD_PART_SIZE_BYTES` - Chunk size for multipart uploads (default: `16777216`, 16 MiB)
- `RETENTION_MAX_BYTES` - Hard cap on `RECORDINGS_DIR`'s total size; once exceeded, sessions are deleted oldest first (preferring already-uploaded ones, but not-yet-uploaded ones too if that's what it takes) until it isn't (default: `107374182400`, 100 GiB; `0` disables cap enforcement — see "Retention & the catch-up/cap sweep" below)
- `RETENTION_SWEEP_INTERVAL` - How often the retention sweep looks for finished-but-not-yet-uploaded sessions and, if over the cap, deletes uploaded ones (default: `5m`)

CycloneDDS domain IDs (not endpoints/addresses) are how independent DDS
"networks" on the same host/subnet are separated — leave at the default `0`
unless the robot's network config uses a non-default domain. To reach the
robot on a specific network interface, or a non-default domain, set
`CYCLONEDDS_URI` per [CycloneDDS's config
documentation](https://cyclonedds.io/docs/cyclonedds/latest/config/) rather
than anything in this table — it's read directly by libddsc, not this
recorder.

## Building

```bash
make build
```

The first run builds CycloneDDS from source (see "Build prerequisites"
above) — a few minutes; subsequent runs reuse the cached `.cyclonedds/install`.

The binary will be created at `bin/om1-telemetry`.

## Running

```bash
./bin/om1-telemetry
```

Or with custom settings:

```bash
ROBOT_TYPE=g1 \
ENABLE_COLLECTION=true \
FRONT_CAMERA_RAW_RTSP_URL="rtsp://localhost:8556/raw" \
REAR_CAMERA_RAW_RTSP_URL="rtsp://localhost:8558/raw" \
AUDIO_RTSP_URL="rtsp://localhost:8555/live" \
VIDEO_FEATURES_LOG="/shared/video-processor/features.jsonl" \
LIDAR_DDS_TOPIC="rt/scan" \
POINTCLOUD_DDS_TOPIC="rt/utlidar/cloud_livox_mid360" \
DEPTH_DDS_TOPIC="rt/camera/realsense2_camera_node/depth/image_rect_raw" \
ODOM_DDS_TOPIC="rt/odom" \
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
        ├── meta.json                  # Session metadata (start/end time, boot id, clock state)
        ├── clock_timebase.jsonl       # Monotonic<->UTC journal; see "Recording without a clock"
        ├── front_camera_raw.mp4       # G1: front pre-CV camera recording
        ├── rear_camera_raw.mp4        # G1: rear pre-CV camera recording
        ├── front_camera_raw_frames.csv  # Per-frame: ...,wallclock_unix_ns,mono_ns
        ├── audio.ogg                  # Audio recording
        ├── video_features.jsonl       # Verbatim video-processor feature events (if VIDEO_FEATURES_LOG set)
        ├── video_features_timestamps.csv  # unix_ns,t_capture_ns,type,seq (capture time)
        ├── video_features_timebase.json   # monotonic<->UTC mapping from the log header
        ├── lidar_scans.bin            # Raw lidar point cloud data
        ├── lidar_timestamps.csv       # Timestamps: unix_ns,seq,byte_offset,mono_ns
        ├── pointcloud_frames.bin      # zstd-compressed Livox MID360 PointCloud2 frames
        ├── pointcloud_timestamps.csv  # unix_ns,seq,byte_offset,byte_length,method,mono_ns
        ├── depth_frames.bin           # RVL-compressed depth frames (lossless)
        ├── depth_timestamps.csv       # unix_ns,seq,byte_offset,byte_length,method,width,height,encoding,mono_ns
        ├── odom_frames.bin            # Raw odometry messages
        └── odom_timestamps.csv        # Timestamps: unix_ns,seq,byte_offset,mono_ns
```

Every timestamps CSV carries a **`mono_ns`** column alongside its wall-clock
column. On a Go2, `top_camera.mp4` / `front_camera.mp4` / `down_camera.mp4`
appear instead of the two G1 raw streams.

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

### Recording without a clock

A robot that boots away from a network does not know the time. The G1's RTC is
the PMIC's and is not battery-backed, so a cold boot comes up at whatever was
last persisted — days stale in the worst case — and with no network, NTP never
corrects it. `docker.service` is ordered `After=time-set.target`, which only
means the clock has been set to *something*; it is not `time-sync.target`. So
the recorder can and does start while the clock is wrong.

Previously that meant one `time.Now()` at startup named the session directory,
NTP corrected the clock seconds later, and every subsequent byte was written
into a directory dated days in the past. Timestamps *inside* the files jumped
mid-session too, leaving them non-monotonic.

The recorder now separates the two clocks:

- **`unix_ns`** — the wall clock. Wrong while the robot is offline.
- **`mono_ns`** — `CLOCK_BOOTTIME`. Never jumps when NTP steps the clock, and
  keeps counting across suspend. Correct from the first row.

While the clock is untrustworthy the session is written to
`recordings/pending/<boot-id>_<uptime-ms>/`, named from facts that hold
regardless of the date, and recorders write through a `recordings/.current`
symlink. `clock_timebase.jsonl` journals a record at startup, on every clock
step, and once a minute.

**When NTP synchronizes, everything is corrected automatically**, with no
interruption to any recorder:

1. The step is journaled with the exact offset.
2. The true session start is back-computed along the monotonic timeline.
3. The directory is renamed to its real date, and the symlink repointed —
   atomically. Open file descriptors follow the inode; new segments open
   through the symlink and land in the new directory. Neither notices.

If the process dies before that (crash, power cut), the next start sweeps
`pending/` and dates any session whose journal contains a synchronized record.

A session that is **never** online from boot to shutdown stays in `pending/`,
permanently. Its monotonic timeline ended with that boot, so no later sync can
date it — the information does not exist. Leaving it undated is deliberate;
inventing a date would be worse. Installing `fake-hwclock` on the host bounds
that error to "since the last shutdown" instead of "since whenever".

Detecting all this needs `/run/systemd/timesync` mounted read-only into the
container (see `om1_telemetry.yml`). Without it the recorder cannot distinguish
a stale clock from a good one and behaves exactly as it did before — dating
every session directly.

#### Correcting timestamps afterwards

`align_recording.py` and `verify_recording.py` apply the correction when they
read a session, so you normally do not need to do anything. The CSVs on disk are
left as recorded on purpose: the recording should say what was observed.

For a downstream tool that reads `unix_ns` directly:

```bash
./script/fix_session_time.py /path/to/session              # report only
./script/fix_session_time.py /path/to/session --write      # corrected copies
./script/fix_session_time.py /path/to/session --in-place   # rewrite, originals kept in <col>_recorded
./script/fix_session_time.py recordings/ --all             # sweep every session
```

The correction is a per-row offset — the sum of every step that happened after
that row was written — not a recomputed value. That preserves what each column
meant: a Zenoh row's `unix_ns` is the *publisher's* stamp and `mono_ns` is when
this recorder received it, so the transport latency between them survives.

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

## Session rotation & cloud upload

By default (`SESSION_ROTATE_INTERVAL=5m`) the recorder does not run one
session for its whole lifetime: every 5 minutes it stops all streams, closes
the current session, and opens a fresh one, so each 5-minute segment can be
uploaded independently instead of waiting for the whole run (which, for a
long-lived robot process, would mean never). Recording resumes within
roughly a second — ffmpeg/DDS reconnect as part of opening the next segment's
streams; there is a small gap in each stream at every rotation boundary, not
a seamless splice.

Set `OPENMIND_API_URL` and `OPENMIND_API_KEY` to upload each rotated-away
segment to the [openmind-api](https://github.com/OpenMind/openmind-api)'s
data-collection endpoints (`internal/handlers/data_collection_upload.go`
there) once it closes — a presigned S3 POST for ordinary files, true S3
multipart upload for anything at or above `UPLOAD_MULTIPART_THRESHOLD_BYTES`.
Uploading is best-effort and runs in the background while the next segment
records: a failed upload marks the session `failed` server-side and leaves
the local files untouched, so a later run — the openmind-api resumes an
upload against the same `session_dir` instead of duplicating it — or a
manual retry can pick it back up. `DELETE_AFTER_UPLOAD=true` removes a
segment's local files once (and only once) its upload has actually
succeeded; it stays off by default; a robot's disk is cheap compared to
losing data to a bug in a new upload path.

Without both `OPENMIND_API_URL` and `OPENMIND_API_KEY` set, rotation still
happens (segmenting the recording locally) but nothing is uploaded --
`UPLOAD_ENABLED` (default `true`) is the operator's intent, actual uploading
additionally requires both to be set, and the recorder logs a warning once
at startup if it's on but not fully configured.

### Preprocessing before upload

`internal/upload/preprocess.go` runs a small, fixed pipeline over a
segment's directory once it's closed and just before its files are
uploaded — this is the one point where the whole segment is known to be
final and static, so it's where any transform that needs the finished
segment (not the live, still-being-written one) belongs. Steps run in a
list (`preprocessSteps`) and must each be idempotent, since a failed upload
leaves the segment on disk for a later retry that re-runs the same pipeline.
Add new steps here as more preprocessing is needed.

The only step today, `convertJSONLToJSON`, rewrites every `*.jsonl` file
(`clock_timebase.jsonl`, and `video_features.jsonl` if enabled) into a
same-named `*.json` holding a single JSON array of its records, then
removes the `.jsonl`. Recording itself keeps writing JSONL — it's an
append-only log, written incrementally and in the features case while it's
being tailed live, so it needs every completed line to stay valid even if
the process dies mid-write, a property a single JSON document doesn't have.
But openmind-api's S3 bucket policy only allows a fixed extension
whitelist that doesn't include `.jsonl`, so the now-static log gets
converted to one real JSON document right before it's uploaded.

### Retention & the catch-up/cap sweep

The rotation and shutdown upload paths above only ever handle the one
segment that just closed, and never retry it if that upload fails — a robot
that's offline for an hour, or a run that crashes mid-upload, would
otherwise leave that data stuck locally forever. `cmd/main/retention.go`
runs a separate sweep, every `RETENTION_SWEEP_INTERVAL`, that:

1. If upload is configured, walks every closed session directory, oldest
   first, and uploads any that were never confirmed uploaded — the same
   idempotent `UploadSession` call rotation uses, so a partially-uploaded
   segment resumes rather than duplicating. A directory is marked uploaded
   (a `.uploaded` file inside it, never itself uploaded) only after that
   call succeeds.
2. If `RECORDINGS_DIR`'s total size is still over `RETENTION_MAX_BYTES`
   afterward, deletes directories oldest first until it isn't — preferring
   already-uploaded ones, but falling through to not-yet-uploaded ones if
   deleting only uploaded directories isn't enough (or none exist, e.g.
   upload isn't configured at all).

It never touches the session currently being recorded to, or whichever
segment still physically holds the live `clock_timebase.jsonl` journal (see
"Interaction with clock trust" below). Otherwise, `RETENTION_MAX_BYTES` is a
hard cap: a robot that's been offline, or has upload turned off entirely,
still gets its oldest data deleted rather than filling its disk and
silently stopping recording — losing the oldest, least-recoverable segment
is treated as better than that.

### Interaction with clock trust

Each rotated segment is dated (or left in `pending/`) the same way the very
first session is -- see "Recording without a clock" above -- based on the
clock's *current* sync state, not the state at process boot: once NTP has
synchronized, every later segment is dated directly even on a run that
booted offline. Only the segment that is actually live at the moment the
clock synchronizes gets promoted out of `pending/`; an earlier segment
rotated away before that moment stays undated, on purpose, for the same
reason a `pending/` session from a boot that was never online stays undated
(see above): its monotonic timeline has nothing to anchor it to UTC.

`clock_timebase.jsonl` is a single, boot-relative journal (one clock, one
set of step/sync events for the whole process), not a per-segment one -- it
physically lives in the first session's directory. Every later segment gets
its own copy of it (a snapshot taken when that segment closes), so each
segment's directory stays self-contained for `align_recording.py` /
`fix_session_time.py` without needing the boot session alongside it.

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
