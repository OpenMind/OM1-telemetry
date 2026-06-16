package lowstate

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse-zenoh/zenoh-go/zenoh"

	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
	"om1-telemetry/internal/zenohutil"
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
	ZenohEndpoint  string
	ZenohTopic     string
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

type SessionResult struct {
	session zenoh.Session
	err     error
}

type SubscriberResult struct {
	subscriber zenoh.Subscriber
	err        error
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
	config := zenoh.NewConfigDefault()
	if err := config.InsertJson5(zenoh.ConfigModeKey, `"client"`); err != nil {
		return err
	}
	endpoint := l.cfg.ZenohEndpoint
	if err := config.InsertJson5(zenoh.ConfigConnectKey, `["`+endpoint+`"]`); err != nil {
		return fmt.Errorf("set connect endpoint: %w", err)
	}

	sessionChan := make(chan SessionResult, 1)
	go func() {
		session, err := zenoh.Open(config, nil)
		sessionChan <- SessionResult{session, err}
	}()

	var session zenoh.Session
	select {
	case <-ctx.Done():
		return ctx.Err()
	case result := <-sessionChan:
		if result.err != nil {
			return fmt.Errorf("open zenoh session: %w", result.err)
		}
		session = result.session
	}
	defer session.Drop()

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

	keyExpr, err := zenoh.NewKeyExpr(l.cfg.ZenohTopic)
	if err != nil {
		return fmt.Errorf("create key expression: %w", err)
	}

	// FIFO buffer sized for high-frequency data.
	// G1 @ 1053 Hz: 2048 entries ≈ 2 s of slack
	// Go2 @ 500 Hz: 2048 entries ≈ 4 s of slack
	handler := zenoh.NewFifoChannel[zenoh.Sample](2048)

	subscriberChan := make(chan SubscriberResult, 1)
	go func() {
		subscriber, err := session.DeclareSubscriber(keyExpr, handler, nil)
		subscriberChan <- SubscriberResult{subscriber, err}
	}()

	var subscriber zenoh.Subscriber
	select {
	case <-ctx.Done():
		return ctx.Err()
	case result := <-subscriberChan:
		if result.err != nil {
			return fmt.Errorf("declare subscriber: %w", result.err)
		}
		subscriber = result.subscriber
	}
	defer subscriber.Drop()

	slog.Info("lowstate recorder started", "topic", l.cfg.ZenohTopic)

	receiver := subscriber.Handler()

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
				return fmt.Errorf("subscriber channel closed")
			}

			var unixNs int64
			tsOpt := sample.TimeStamp()
			if tsOpt.IsSome() {
				ts := tsOpt.Unwrap()
				unixNs = zenohutil.TimestampToUnixNs(ts)
			} else {
				unixNs = time.Now().UnixNano()
			}

			payload := sample.Payload()
			n, err := dataFile.Write(payload.Bytes())
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
