package video

import (
	"context"
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
	file, start, end, ok := parseSegmentListLine("20260826T185832.mp4,12.500000,15.000000")
	require.True(t, ok)
	require.Equal(t, "20260826T185832.mp4", file)
	require.InDelta(t, 12.5, start, 1e-9)
	require.InDelta(t, 15.0, end, 1e-9)
}

func TestParseSegmentListLine_rejectsMalformedLine(t *testing.T) {
	_, _, _, ok := parseSegmentListLine("not,a,valid,,line")
	require.False(t, ok, "a non-numeric start field must be rejected")

	_, _, _, ok = parseSegmentListLine("onlyonefield")
	require.False(t, ok, "a line missing the start/end fields must be rejected")
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
	processStart := time.Unix(1_800_000_000, 0)
	stream.Rotate(processStart, filepath.Join(sessionDir, "front_camera_timestamps.csv"), "") // no frames file: skip async ffprobe

	segmentStart := processStart.Add(12500 * time.Millisecond)
	stream.finishSegment(scratchFile, 0, segmentStart, 5_000_000_000)

	wantFinal := filepath.Join(sessionDir, "front_camera_20260826T185832.mp4")
	require.FileExists(t, wantFinal, "the segment must be relocated with its camera stem prefixed")
	require.NoFileExists(t, scratchFile, "the scratch copy must be gone after relocation")

	data, err := os.ReadFile(filepath.Join(sessionDir, "front_camera_timestamps.csv"))
	require.NoError(t, err)
	content := string(data)
	require.Contains(t, content, "recording_start_unix_ns,segment_file,mono_ns")
	require.Contains(t, content, fmt.Sprintf("%d", segmentStart.UnixNano()), "the indexed start time must match the segment's own start")
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
	stream.Rotate(time.Now(), filepath.Join(sessionDir, "front_camera_timestamps.csv"), "")

	stream.watchSegments(strings.NewReader("20260826T185832.mp4,0.000000,3.000000\n"))

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

	stream.Rotate(time.Now(), filepath.Join(secondDir, "front_camera_timestamps.csv"), "")

	seg2 := filepath.Join(scratchDir, "seg2.mp4")
	require.NoError(t, os.WriteFile(seg2, []byte("b"), 0o644))
	stream.finishSegment(seg2, 0, time.Now(), 0)
	require.FileExists(t, filepath.Join(secondDir, "front_camera_seg2.mp4"), "after Rotate, new segments must land in the new session directory")
}

// Regression: targetFor must attribute by the segment's own timestamp even
// when Rotate for the next session has already run.
func TestTargetFor_attributesByOwnTimestampEvenWhenRotateRacesAhead(t *testing.T) {
	scratchDir := t.TempDir()
	sessionNDir := t.TempDir()
	sessionN1Dir := t.TempDir()

	sessionNStart := time.Unix(1_800_000_000, 0)
	sessionN1Start := sessionNStart.Add(10 * time.Second)

	stream := New(Config{
		RTSPURL:        "rtsp://192.0.2.1:8554/unreachable",
		OutputFile:     filepath.Join(t.TempDir(), "front_camera.mp4"),
		TimestampsFile: filepath.Join(sessionNDir, "front_camera_timestamps.csv"),
		SessionStart:   sessionNStart,
		ScratchDir:     scratchDir,
	})
	stream.ensureTarget()

	stream.Rotate(sessionN1Start, filepath.Join(sessionN1Dir, "front_camera_timestamps.csv"), "")

	seg := filepath.Join(scratchDir, "seg.mp4")
	require.NoError(t, os.WriteFile(seg, []byte("a"), 0o644))

	stream.finishSegment(seg, 0, sessionNStart.Add(5*time.Second), 0)

	require.FileExists(t, filepath.Join(sessionNDir, "front_camera_seg.mp4"),
		"a segment that started during session N must land in session N's directory, even if Rotate for N+1 already ran")
	require.NoFileExists(t, filepath.Join(sessionN1Dir, "front_camera_seg.mp4"))
}

// Regression: a segment starting just after a rotation boundary must land
// in the new session even after a long-running process's clock has drifted.
func TestFinishSegment_notMisledByAccumulatedProcessDrift(t *testing.T) {
	scratchDir := t.TempDir()
	sessionNDir := t.TempDir()
	sessionN1Dir := t.TempDir()

	sessionNStart := time.Unix(1_800_000_000, 0)
	sessionN1Start := sessionNStart.Add(5 * time.Minute)

	stream := New(Config{
		RTSPURL:        "rtsp://192.0.2.1:8554/unreachable",
		OutputFile:     filepath.Join(t.TempDir(), "front_camera.mp4"),
		TimestampsFile: filepath.Join(sessionNDir, "front_camera_timestamps.csv"),
		SessionStart:   sessionNStart,
		ScratchDir:     scratchDir,
	})
	stream.ensureTarget()
	stream.Rotate(sessionN1Start, filepath.Join(sessionN1Dir, "front_camera_timestamps.csv"), "")

	segmentDuration := 5 * time.Minute
	trueSegmentStart := sessionN1Start.Add(1 * time.Second)
	observedClose := trueSegmentStart.Add(segmentDuration)

	seg := filepath.Join(scratchDir, "seg.mp4")
	require.NoError(t, os.WriteFile(seg, []byte("a"), 0o644))

	stream.finishSegment(seg, segmentDuration, observedClose, 0)

	require.FileExists(t, filepath.Join(sessionN1Dir, "front_camera_seg.mp4"),
		"a segment observed to close one full span after starting just past the rotation boundary must land in the new session")
	require.NoFileExists(t, filepath.Join(sessionNDir, "front_camera_seg.mp4"))
}

// Regression: an uploader triggered right on rotation must be able to wait
// for the session's own segment to land instead of racing finishSegment.
func TestWaitSegment_blocksUntilFinishSegmentRelocatesThatSessionsSegment(t *testing.T) {
	scratchDir := t.TempDir()
	sessionDir := t.TempDir()
	sessionStart := time.Unix(1_800_000_000, 0)

	stream := New(Config{
		RTSPURL:        "rtsp://192.0.2.1:8554/unreachable",
		OutputFile:     filepath.Join(t.TempDir(), "front_camera.mp4"),
		TimestampsFile: filepath.Join(sessionDir, "front_camera_timestamps.csv"),
		SessionStart:   sessionStart,
		ScratchDir:     scratchDir,
	})
	stream.ensureTarget()

	waited := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		waited <- stream.WaitSegment(ctx, sessionStart)
	}()

	select {
	case <-waited:
		t.Fatal("WaitSegment returned before finishSegment relocated anything")
	case <-time.After(20 * time.Millisecond):
	}

	seg := filepath.Join(scratchDir, "seg.mp4")
	require.NoError(t, os.WriteFile(seg, []byte("a"), 0o644))
	stream.finishSegment(seg, 0, sessionStart, 0)

	require.True(t, <-waited, "WaitSegment must return true once finishSegment relocates the session's segment")
}

// A target that never receives a segment (RTSP never delivered data for
// that session) must not hang WaitSegment past its context.
func TestWaitSegment_returnsFalseWhenContextEndsFirst(t *testing.T) {
	sessionStart := time.Unix(1_800_000_000, 0)
	stream := New(Config{
		RTSPURL:      "rtsp://192.0.2.1:8554/unreachable",
		OutputFile:   filepath.Join(t.TempDir(), "front_camera.mp4"),
		SessionStart: sessionStart,
		ScratchDir:   t.TempDir(),
	})
	stream.ensureTarget()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.False(t, stream.WaitSegment(ctx, sessionStart))
}

// A relocation failure must still unblock waiters rather than leaving them
// to time out every time.
func TestWaitSegment_returnsTrueImmediatelyAfterRelocationFailure(t *testing.T) {
	sessionDir := t.TempDir()
	sessionStart := time.Unix(1_800_000_000, 0)
	stream := New(Config{
		RTSPURL:        "rtsp://192.0.2.1:8554/unreachable",
		OutputFile:     filepath.Join(t.TempDir(), "front_camera.mp4"),
		TimestampsFile: filepath.Join(sessionDir, "front_camera_timestamps.csv"),
		SessionStart:   sessionStart,
		ScratchDir:     t.TempDir(),
	})
	stream.ensureTarget()

	// scratchFile does not exist, so os.Rename inside finishSegment fails.
	stream.finishSegment(filepath.Join(t.TempDir(), "missing.mp4"), 0, sessionStart, 0)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.True(t, stream.WaitSegment(ctx, sessionStart),
		"a relocation failure must still mark the target ready so callers don't wait out the full timeout")
}

// No target was ever installed for this sessionStart.
func TestWaitSegment_returnsFalseForUnknownSessionStart(t *testing.T) {
	stream := New(Config{
		RTSPURL:    "rtsp://192.0.2.1:8554/unreachable",
		OutputFile: filepath.Join(t.TempDir(), "front_camera.mp4"),
		ScratchDir: t.TempDir(),
	})
	stream.ensureTarget()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.False(t, stream.WaitSegment(ctx, time.Unix(1_900_000_000, 0)))
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
