package lidar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"om1-telemetry/internal/ddscore"
	"om1-telemetry/internal/lidar/ddstest"
)

func countDataLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read %s: %v", path, err)
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l != "" {
			n++
		}
	}
	return n
}

func waitForLines(t *testing.T, path string, want int, timeout time.Duration, msgAndArgs ...any) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if countDataLines(t, path) >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d line(s) in %s (have %d): %v",
				want, path, countDataLines(t, path), msgAndArgs)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Reconnect exists for exactly one real-world scenario: the physical sensor
// (or its driver process) restarts, the reader that was already running
// misses the writer's post-restart discovery burst, and -- unlike a brand
// new reader, which would match immediately -- it never recovers on its
// own. This test doesn't reproduce a lost discovery packet (not practical
// over real loopback DDS), but it does prove the mechanical half of the
// fix: Reconnect() tears the subscription down and rebuilds it without
// losing the stream's ability to receive -- so whatever heartbeat.Monitor
// decides to call it on gets a working reader back.
func TestReconnect_stillReceivesAfterRecreatingSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DDS loopback test in short mode")
	}

	t.Setenv("CYCLONEDDS_URI", `<CycloneDDS><Domain><General><Interfaces><NetworkInterface name="lo" multicast="true"/></Interfaces></General></Domain></CycloneDDS>`)

	const (
		domainID  = 79
		topicName = "reconnect_test_scan"
	)

	wp, err := ddscore.NewParticipant(domainID)
	require.NoError(t, err)
	defer func() { _ = wp.Close() }()

	writer, err := ddstest.SetupWriter(wp, topicName)
	require.NoError(t, err)

	dir := t.TempDir()
	tsFile := filepath.Join(dir, "timestamps.csv")
	stream := New(Config{
		DDSDomainID:    domainID,
		DDSTopic:       topicName,
		TimestampsFile: tsFile,
		DataFile:       filepath.Join(dir, "data.bin"),
	})
	stream.Start()
	defer stream.Stop()

	require.NoError(t, ddstest.PublishScan(writer, 8, 0))
	waitForLines(t, tsFile, 1, 2*time.Second) // +1 for the CSV header

	stream.Reconnect()
	// Give record() time to observe the request and tear down the old
	// participant/reader before the recreated one starts discovery.
	time.Sleep(300 * time.Millisecond)

	// The recreated reader still has to complete DDS discovery with the
	// (already-running) writer before a sample lands, and a single publish
	// racing that handshake could be missed entirely (VOLATILE durability
	// delivers only to readers already matched at publish time) -- so
	// publish repeatedly, like the real writer's continuous stream would,
	// until the new subscription has clearly caught up.
	deadline := time.After(5 * time.Second)
	for countDataLines(t, tsFile) < 2 {
		require.NoError(t, ddstest.PublishScan(writer, 8, 1))
		select {
		case <-deadline:
			t.Fatalf("a sample published after Reconnect was never received by the recreated subscription (have %d line(s))",
				countDataLines(t, tsFile))
		case <-time.After(100 * time.Millisecond):
		}
	}
}
