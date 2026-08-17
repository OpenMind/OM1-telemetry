package pointcloud

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"om1-telemetry/internal/clock"
	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
)

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
const HeartbeatName = "pointcloud"

const syncInterval = 2 * time.Second

type Config struct {
	DDSDomainID    uint32
	DDSTopic       string
	TimestampsFile string
	DataFile       string

	// Monitor is optional; ticks once per stored frame so the
	// central heartbeat monitor can detect a stuck recorder.
	Monitor *heartbeat.Monitor
}

type PointCloudStream struct {
	cfg     Config
	running atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}
	wg      sync.WaitGroup
}

func New(cfg Config) *PointCloudStream {
	return &PointCloudStream{cfg: cfg}
}

func (c *PointCloudStream) Start() {
	if c.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan struct{})
	c.wg.Add(1)
	go c.loop(ctx)
}

func (c *PointCloudStream) Stop() {
	if !c.running.Swap(false) {
		return
	}
	c.cancel()
	c.wg.Wait()
	close(c.done)
	slog.Info("pointcloud stream stopped")
}

func (c *PointCloudStream) loop(ctx context.Context) {
	defer c.wg.Done()
	for ctx.Err() == nil {
		if err := c.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("pointcloud recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (c *PointCloudStream) record(ctx context.Context) error {
	receiver, closeSub, err := subscribeDDS(ctx, c.cfg.DDSDomainID, c.cfg.DDSTopic)
	if err != nil {
		return fmt.Errorf("subscribe dds: %w", err)
	}
	defer closeSub()

	// Open files in APPEND mode so reconnects do NOT clobber existing data.
	tsResult, err := recordutil.OpenForAppend(c.cfg.TimestampsFile)
	if err != nil {
		return fmt.Errorf("open timestamps file: %w", err)
	}
	tsFile := tsResult.File
	defer func() {
		if err := tsFile.Close(); err != nil {
			slog.Error("failed to close timestamps file", "err", err)
		}
	}()

	dataResult, err := recordutil.OpenForAppend(c.cfg.DataFile)
	if err != nil {
		return fmt.Errorf("open data file: %w", err)
	}
	dataFile := dataResult.File
	defer func() {
		if err := dataFile.Close(); err != nil {
			slog.Error("failed to close data file", "err", err)
		}
	}()

	// Only write CSV header on first open (empty file).
	if tsResult.PrevSize == 0 {
		if _, err := fmt.Fprintln(tsFile,
			"unix_ns,seq,byte_offset,byte_length,method,mono_ns"); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
	}

	// Continue counters from where the previous session left off.
	lastSeq, err := recordutil.ReadLastSeq(c.cfg.TimestampsFile)
	if err != nil {
		slog.Warn("could not read last seq; starting from 0", "err", err)
		lastSeq = -1
	}
	seq := lastSeq + 1
	byteOffset := dataResult.PrevSize

	if dataResult.PrevSize > 0 {
		slog.Info("pointcloud resuming previous session",
			"starting_seq", seq,
			"starting_byte_offset", byteOffset)
	}

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		return fmt.Errorf("create zstd encoder: %w", err)
	}
	defer func() {
		if err := encoder.Close(); err != nil {
			slog.Error("failed to close zstd encoder", "err", err)
		}
	}()

	slog.Info("pointcloud recorder started", "domain", c.cfg.DDSDomainID, "topic", c.cfg.DDSTopic)

	// Periodic fsync so a crash never loses more than syncInterval of data.
	syncTicker := time.NewTicker(syncInterval)
	defer syncTicker.Stop()

	flush := func() {
		if err := dataFile.Sync(); err != nil {
			slog.Warn("pointcloud data sync failed", "err", err)
		}
		if err := tsFile.Sync(); err != nil {
			slog.Warn("pointcloud ts sync failed", "err", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return nil
		case <-syncTicker.C:
			flush()
		case sample, ok := <-receiver:
			if !ok {
				flush()
				return fmt.Errorf("dds subscriber channel closed")
			}

			unixNs := sample.unixNs
			if unixNs == 0 {
				unixNs = time.Now().UnixNano()
			}
			// Receive time on the boot clock. unixNs above is the publisher's
			// stamp and shares this host's wall clock, so a clock step spoils
			// both; monoNs is immune to the step and says which correction
			// applies to this row. See internal/clock.
			monoNs := clock.MonoNs()

			data, method := encodeFrame(encoder, sample.data)

			n, err := dataFile.Write(data)
			if err != nil {
				return fmt.Errorf("write data: %w", err)
			}

			if _, err := fmt.Fprintf(tsFile, "%d,%d,%d,%d,%s,%d\n",
				unixNs, seq, byteOffset, n, method, monoNs); err != nil {
				return fmt.Errorf("write timestamp: %w", err)
			}

			byteOffset += int64(n)
			seq++

			// Heartbeat tick: safe if Monitor is nil.
			c.cfg.Monitor.Tick(HeartbeatName)
		}
	}
}

func encodeFrame(encoder *zstd.Encoder, raw []byte) (data []byte, method string) {
	compressed := encoder.EncodeAll(raw, make([]byte, 0, len(raw)))
	if len(compressed) >= len(raw) {
		return raw, "raw"
	}
	return compressed, "zstd"
}
