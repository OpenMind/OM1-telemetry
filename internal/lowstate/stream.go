package lowstate

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
)

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
// Same name on Go2 and G1 — the messages have slightly different
// schemas (unitree_go vs unitree_hg) but identical purpose; the data
// pipeline doesn't need to distinguish them.
const HeartbeatName = "lowstate"

// /lowstate is the catch-all robot state message:
// IMU, joint states, battery, foot forces, wireless remote, temperatures, etc.
// We record raw payload bytes + per-message timestamps; downstream tools
// deserialize when needed.
//
// Measured rates:
//   Go2 (unitree_go/msg/LowState):  ~500 Hz,  ~2.5 KB / msg = ~1.25 MB/s
//   G1  (unitree_hg/msg/LowState):  ~1053 Hz, ~2.1 KB / msg = ~2.21 MB/s
//
// Daily (8h continuous): Go2 ~36 GB, G1 ~62 GB

const syncInterval = 2 * time.Second

type Config struct {
	// RobotType selects the LowState DDS schema to subscribe with: "go2"
	// (unitree_go/msg/LowState) or "g1" (unitree_hg/msg/LowState). Anything
	// else defaults to "go2".
	RobotType      string
	DDSDomainID    uint32
	DDSTopic       string
	TimestampsFile string
	DataFile       string

	// Monitor is optional; ticks once per message so the central
	// heartbeat monitor can detect a stuck recorder.
	Monitor *heartbeat.Monitor
}

type LowstateStream struct {
	cfg     Config
	running atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}
	wg      sync.WaitGroup
}

func New(cfg Config) *LowstateStream {
	return &LowstateStream{cfg: cfg}
}

func (l *LowstateStream) Start() {
	if l.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.done = make(chan struct{})
	l.wg.Add(1)
	go l.loop(ctx)
}

func (l *LowstateStream) Stop() {
	if !l.running.Swap(false) {
		return
	}
	l.cancel()
	l.wg.Wait()
	close(l.done)
	slog.Info("lowstate stream stopped")
}

func (l *LowstateStream) loop(ctx context.Context) {
	defer l.wg.Done()
	for ctx.Err() == nil {
		if err := l.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("lowstate recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (l *LowstateStream) record(ctx context.Context) error {
	receiver, closeSub, err := subscribeDDS(ctx, l.cfg.DDSDomainID, l.cfg.DDSTopic, l.cfg.RobotType)
	if err != nil {
		return fmt.Errorf("subscribe dds: %w", err)
	}
	defer closeSub()

	// Append-mode open: do NOT clobber prior data on reconnect.
	tsResult, err := recordutil.OpenForAppend(l.cfg.TimestampsFile)
	if err != nil {
		return fmt.Errorf("open timestamps file: %w", err)
	}
	tsFile := tsResult.File
	defer func() {
		if err := tsFile.Close(); err != nil {
			slog.Error("failed to close timestamps file", "err", err)
		}
	}()

	dataResult, err := recordutil.OpenForAppend(l.cfg.DataFile)
	if err != nil {
		return fmt.Errorf("open data file: %w", err)
	}
	dataFile := dataResult.File
	defer func() {
		if err := dataFile.Close(); err != nil {
			slog.Error("failed to close data file", "err", err)
		}
	}()

	if tsResult.PrevSize == 0 {
		if _, err := fmt.Fprintln(tsFile, "unix_ns,seq,byte_offset"); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
	}

	lastSeq, err := recordutil.ReadLastSeq(l.cfg.TimestampsFile)
	if err != nil {
		slog.Warn("could not read last seq; starting from 0", "err", err)
		lastSeq = -1
	}
	seq := lastSeq + 1
	byteOffset := dataResult.PrevSize

	if dataResult.PrevSize > 0 {
		slog.Info("lowstate resuming previous session",
			"starting_seq", seq,
			"starting_byte_offset", byteOffset)
	}

	slog.Info("lowstate recorder started", "domain", l.cfg.DDSDomainID, "topic", l.cfg.DDSTopic, "robot_type", l.cfg.RobotType)

	syncTicker := time.NewTicker(syncInterval)
	defer syncTicker.Stop()

	flush := func() {
		if err := dataFile.Sync(); err != nil {
			slog.Warn("lowstate data sync failed", "err", err)
		}
		if err := tsFile.Sync(); err != nil {
			slog.Warn("lowstate ts sync failed", "err", err)
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

			n, err := dataFile.Write(sample.data)
			if err != nil {
				return fmt.Errorf("write data: %w", err)
			}

			if _, err := fmt.Fprintf(tsFile, "%d,%d,%d\n", unixNs, seq, byteOffset); err != nil {
				return fmt.Errorf("write timestamp: %w", err)
			}

			byteOffset += int64(n)
			seq++

			// Heartbeat tick. Safe if Monitor is nil.
			l.cfg.Monitor.Tick(HeartbeatName)
		}
	}
}
