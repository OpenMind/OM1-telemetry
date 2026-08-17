package lidar

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"om1-telemetry/internal/ddscore"
	"om1-telemetry/internal/lidar/ddstest"
)

// readVmRSSKB reads RSS from /proc/self/status, since the leaked memory is native C-heap invisible to Go's own stats.
func readVmRSSKB(t *testing.T) uint64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("cannot read /proc/self/status (non-Linux?): %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		return kb
	}
	t.Skip("VmRSS not found in /proc/self/status")
	return 0
}

// TestPollLaserScan_freesNativeSampleContents publishes real samples over an isolated DDS domain and asserts RSS stays flat.
func TestPollLaserScan_freesNativeSampleContents(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DDS loopback leak test in short mode")
	}

	// loopback-only: keeps this test off the robot's real network interface
	t.Setenv("CYCLONEDDS_URI", `<CycloneDDS><Domain><General><Interfaces><NetworkInterface name="lo" multicast="true"/></Interfaces></General></Domain></CycloneDDS>`)

	const (
		domainID   = 77
		topicName  = "leak_test_scan"
		numFloats  = 2000
		iterations = 2000
	)

	wp, err := ddscore.NewParticipant(domainID)
	if err != nil {
		t.Fatalf("writer participant: %v", err)
	}
	defer func() { _ = wp.Close() }()

	writer, err := ddstest.SetupWriter(wp, topicName)
	if err != nil {
		t.Fatalf("setup writer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, closer, err := subscribeDDS(ctx, domainID, topicName)
	if err != nil {
		t.Fatalf("subscribeDDS: %v", err)
	}
	defer closer()

	time.Sleep(300 * time.Millisecond)

	rssBefore := readVmRSSKB(t)

	var lastData []byte
	for i := 0; i < iterations; i++ {
		if err := ddstest.PublishScan(writer, numFloats, int32(i)); err != nil {
			t.Fatalf("publish sample %d: %v", i, err)
		}

		select {
		case rs, ok := <-out:
			if !ok {
				t.Fatalf("reader channel closed early after %d/%d samples", i, iterations)
			}
			lastData = rs.data
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for sample %d/%d", i, iterations)
		}
	}

	rssAfter := readVmRSSKB(t)
	growthKB := int64(rssAfter) - int64(rssBefore)
	t.Logf("RSS before=%dKB after=%dKB growth=%dKB over %d samples (%d floats/sequence)",
		rssBefore, rssAfter, growthKB, iterations, numFloats)

	const maxAllowedGrowthKB = 10 * 1024
	if growthKB > maxAllowedGrowthKB {
		t.Fatalf("RSS grew by %dKB (> %dKB) across %d samples — native DDS sample memory appears to be leaking",
			growthKB, maxAllowedGrowthKB, iterations)
	}

	if len(lastData) == 0 {
		t.Fatal("expected non-empty encoded data for the last received sample")
	}
	if !bytes.Contains(lastData, []byte("leak-test")) {
		t.Fatal("encoded data does not contain the expected frame_id — sample may have been corrupted by freeing before encoding")
	}
}
