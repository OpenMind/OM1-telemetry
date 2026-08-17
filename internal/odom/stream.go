package odom

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"om1-telemetry/internal/clock"
	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
)

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
const HeartbeatName = "odom"

const syncInterval = 2 * time.Second

type Config struct {
	DDSDomainID    uint32
	DDSTopic       string
	TimestampsFile string
	DataFile       string

	// Monitor is optional; ticks once per message so the central
	// heartbeat monitor can detect a stuck recorder.
	Monitor *heartbeat.Monitor
}

type OdomStream struct {
	cfg     Config
	running atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}
	wg      sync.WaitGroup
}

func New(cfg Config) *OdomStream {
	return &OdomStream{cfg: cfg}
}

func (o *OdomStream) Start() {
	if o.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	o.cancel = cancel
	o.done = make(chan struct{})
	o.wg.Add(1)
	go o.loop(ctx)
}

func (o *OdomStream) Stop() {
	if !o.running.Swap(false) {
		return
	}
	o.cancel()
	o.wg.Wait()
	close(o.done)
	slog.Info("odom stream stopped")
}

func (o *OdomStream) loop(ctx context.Context) {
	defer o.wg.Done()
	for ctx.Err() == nil {
		if err := o.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("odom recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (o *OdomStream) record(ctx context.Context) error {
	receiver, closeSub, err := subscribeDDS(ctx, o.cfg.DDSDomainID, o.cfg.DDSTopic)
	if err != nil {
		return fmt.Errorf("subscribe dds: %w", err)
	}
	defer closeSub()

	// Open files in APPEND mode so reconnects do NOT wipe data.
	tsResult, err := recordutil.OpenForAppend(o.cfg.TimestampsFile)
	if err != nil {
		return fmt.Errorf("open timestamps file: %w", err)
	}
	tsFile := tsResult.File
	defer func() {
		if err := tsFile.Close(); err != nil {
			slog.Error("failed to close timestamps file", "err", err)
		}
	}()

	dataResult, err := recordutil.OpenForAppend(o.cfg.DataFile)
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
		if _, err := fmt.Fprintln(tsFile, "unix_ns,seq,byte_offset,mono_ns"); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
	}

	lastSeq, err := recordutil.ReadLastSeq(o.cfg.TimestampsFile)
	if err != nil {
		slog.Warn("could not read last seq; starting from 0", "err", err)
		lastSeq = -1
	}
	seq := lastSeq + 1
	byteOffset := dataResult.PrevSize

	if dataResult.PrevSize > 0 {
		slog.Info("odom resuming previous session",
			"starting_seq", seq,
			"starting_byte_offset", byteOffset)
	}

	slog.Info("odom recorder started", "domain", o.cfg.DDSDomainID, "topic", o.cfg.DDSTopic)

	syncTicker := time.NewTicker(syncInterval)
	defer syncTicker.Stop()

	flush := func() {
		if err := dataFile.Sync(); err != nil {
			slog.Warn("odom data sync failed", "err", err)
		}
		if err := tsFile.Sync(); err != nil {
			slog.Warn("odom ts sync failed", "err", err)
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
			// Boot-clock receive time. unixNs above is the publisher's stamp,
			// taken on this host's wall clock, so a clock step spoils it;
			// monoNs is immune and says which correction applies to this row.
			// See internal/clock.
			monoNs := clock.MonoNs()

			n, err := dataFile.Write(sample.data)
			if err != nil {
				return fmt.Errorf("write data: %w", err)
			}

			if _, err := fmt.Fprintf(tsFile, "%d,%d,%d,%d\n", unixNs, seq, byteOffset, monoNs); err != nil {
				return fmt.Errorf("write timestamp: %w", err)
			}

			byteOffset += int64(n)
			seq++

			// Heartbeat tick: safe if Monitor is nil.
			o.cfg.Monitor.Tick(HeartbeatName)
		}
	}
}
