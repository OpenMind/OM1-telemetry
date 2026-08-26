package video

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNew_returnsNonNilStream(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://localhost:8554/live",
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
	})
	require.NotNil(t, stream, "New() returned nil")
}

func TestNew_defaultHeartbeatName(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://localhost:8554/live",
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
		// HeartbeatName intentionally empty → should default to HeartbeatName constant
	})
	require.Equal(t, HeartbeatName, stream.cfg.HeartbeatName)
}

func TestNew_customHeartbeatName(t *testing.T) {
	stream := New(Config{
		RTSPURL:       "rtsp://localhost:8554/live",
		OutputFile:    filepath.Join(t.TempDir(), "video.mp4"),
		HeartbeatName: "video_top",
	})
	require.Equal(t, "video_top", stream.cfg.HeartbeatName)
}

func TestStartStop_cleanLifecycle(t *testing.T) {

	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
	})

	stream.Start()

	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		stream.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "Stop() did not return within 5 s")
	}
}

func TestStart_idempotent(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
	})

	stream.Start()
	stream.Start()

	stream.Stop()
}

func TestStop_beforeStart_isNoOp(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
	})
	stream.Stop()
}

func TestStop_idempotent(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
	})

	stream.Start()
	stream.Stop()
	stream.Stop()
}

func TestRecord_createsOutputFileDirectory(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "nested", "session")
	outputFile := filepath.Join(outputDir, "video.mp4")
	scratchDir := filepath.Join(t.TempDir(), "scratch")

	err := os.MkdirAll(outputDir, 0o755)
	require.NoError(t, err, "could not create output dir")

	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: outputFile,
		ScratchDir: scratchDir,
	})

	stream.Start()
	time.Sleep(50 * time.Millisecond)
	stream.Stop()

	require.DirExists(t, scratchDir, "record() must create its scratch dir before invoking ffmpeg")
	_, err = os.Stat(outputDir)
	require.False(t, os.IsNotExist(err), "output directory was unexpectedly removed")
}

func TestParseSegmentListLine_parsesFilenameAndStart(t *testing.T) {
	file, start, ok := parseSegmentListLine("20260826T185832.mp4,12.500000,15.000000")
	require.True(t, ok)
	require.Equal(t, "20260826T185832.mp4", file)
	require.InDelta(t, 12.5, start, 1e-9)
}

func TestParseSegmentListLine_rejectsMalformedLine(t *testing.T) {
	_, _, ok := parseSegmentListLine("not,a,valid,,line")
	require.False(t, ok, "a non-numeric start field must be rejected")

	_, _, ok = parseSegmentListLine("onlyonefield")
	require.False(t, ok, "a line missing the start field must be rejected")
}

func TestFinishSegment_relocatesFileIndexesItAndPrefixesWithStem(t *testing.T) {
	scratchDir := t.TempDir()
	sessionDir := t.TempDir()

	scratchFile := filepath.Join(scratchDir, "20260826T185832.mp4")
	require.NoError(t, os.WriteFile(scratchFile, []byte("fake mp4 data"), 0o644))

	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "front_camera.mp4"),
		ScratchDir: scratchDir,
	})
	stream.Rotate(filepath.Join(sessionDir, "front_camera_timestamps.csv"), "") // no frames file: skip async ffprobe

	processStart := time.Unix(1_800_000_000, 0)
	stream.finishSegment(scratchFile, 12.5, processStart, 5_000_000_000)

	wantFinal := filepath.Join(sessionDir, "front_camera_20260826T185832.mp4")
	require.FileExists(t, wantFinal, "the segment must be relocated with its camera stem prefixed")
	require.NoFileExists(t, scratchFile, "the scratch copy must be gone after relocation")

	data, err := os.ReadFile(filepath.Join(sessionDir, "front_camera_timestamps.csv"))
	require.NoError(t, err)
	content := string(data)
	require.Contains(t, content, "recording_start_unix_ns,segment_file,mono_ns")
	wantStartUnixNs := processStart.Add(12500 * time.Millisecond).UnixNano()
	require.Contains(t, content, fmt.Sprintf("%d", wantStartUnixNs), "the indexed start time must include the segment's offset into the stream")
	require.Contains(t, content, "front_camera_20260826T185832.mp4")
}

// Regression: ffmpeg's segment_list reports each segment's filename as a
// bare basename (no directory), matching how it wrote the pattern -- not a
// path scratchFile can be renamed from directly.
func TestWatchSegments_joinsBareFilenameFromSegmentListWithScratchDir(t *testing.T) {
	scratchDir := t.TempDir()
	sessionDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(scratchDir, "20260826T185832.mp4"), []byte("data"), 0o644))

	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "front_camera.mp4"),
		ScratchDir: scratchDir,
	})
	stream.Rotate(filepath.Join(sessionDir, "front_camera_timestamps.csv"), "")

	processStart := time.Unix(1_800_000_000, 0)
	stream.watchSegments(strings.NewReader("20260826T185832.mp4,0.000000,3.000000\n"), processStart, 0)

	require.FileExists(t, filepath.Join(sessionDir, "front_camera_20260826T185832.mp4"),
		"watchSegments must join the segment_list's bare filename with ScratchDir before relocating")
}

func TestRotate_installsNewTargetForSubsequentSegments(t *testing.T) {
	scratchDir := t.TempDir()
	firstDir := t.TempDir()
	secondDir := t.TempDir()

	stream := New(Config{
		RTSPURL:        "rtsp://192.0.2.1:8554/unreachable",
		OutputFile:     filepath.Join(t.TempDir(), "front_camera.mp4"),
		TimestampsFile: filepath.Join(firstDir, "front_camera_timestamps.csv"),
		ScratchDir:     scratchDir,
	})
	stream.ensureTarget() // simulate loop()'s startup, without needing a real ffmpeg process

	seg1 := filepath.Join(scratchDir, "seg1.mp4")
	require.NoError(t, os.WriteFile(seg1, []byte("a"), 0o644))
	stream.finishSegment(seg1, 0, time.Now(), 0)
	require.FileExists(t, filepath.Join(firstDir, "front_camera_seg1.mp4"))

	stream.Rotate(filepath.Join(secondDir, "front_camera_timestamps.csv"), "")

	seg2 := filepath.Join(scratchDir, "seg2.mp4")
	require.NoError(t, os.WriteFile(seg2, []byte("b"), 0o644))
	stream.finishSegment(seg2, 0, time.Now(), 0)
	require.FileExists(t, filepath.Join(secondDir, "front_camera_seg2.mp4"), "after Rotate, new segments must land in the new session directory")
}

func TestAppendSegmentEntry_emptyPath_isNoOp(t *testing.T) {
	err := appendSegmentEntry("", time.Now(), 0, "/data/video.mp4")
	require.NoError(t, err)
}

func TestAppendSegmentEntry_writesHeaderAndEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "video_timestamps.csv")
	start := time.Unix(1_000_000_000, 123_456_789)
	segFile := "/data/top_camera_20260612T164629_876543210Z.mp4"

	err := appendSegmentEntry(path, start, 12345, segFile)
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	require.Contains(t, content, "recording_start_unix_ns,segment_file,mono_ns")
	require.Contains(t, content, fmt.Sprintf("%d", start.UnixNano()))
	require.Contains(t, content, filepath.Base(segFile))
}

func TestAppendSegmentEntry_headerWrittenOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "video_timestamps.csv")
	seg1 := "/data/video_seg1.mp4"
	seg2 := "/data/video_seg2.mp4"

	require.NoError(t, appendSegmentEntry(path, time.Unix(1_000_000_000, 0), 1, seg1))
	require.NoError(t, appendSegmentEntry(path, time.Unix(2_000_000_000, 0), 2, seg2))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)

	require.Equal(t, 1, strings.Count(content, "recording_start_unix_ns"), "header must appear exactly once")
	require.Contains(t, content, filepath.Base(seg1))
	require.Contains(t, content, filepath.Base(seg2))
}
