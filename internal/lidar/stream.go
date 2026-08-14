package lidar

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse-zenoh/zenoh-go/zenoh"

	"om1-telemetry/internal/clock"
	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
	"om1-telemetry/internal/zenohutil"
)

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
// Register it in main.go as: mon.Register(lidar.HeartbeatName, 10)
const HeartbeatName = "lidar"

const syncInterval = 2 * time.Second

type Config struct {
	ZenohEndpoint  string
	ZenohTopic     string
	TimestampsFile string
	DataFile       string

	// Monitor is optional; if nil, no heartbeat reporting happens.
	Monitor *heartbeat.Monitor
}

type LidarStream struct {
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

func New(cfg Config) *LidarStream {
	return &LidarStream{cfg: cfg}
}

func (l *LidarStream) Start() {
	if l.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.done = make(chan struct{})
	l.wg.Add(1)
	go l.loop(ctx)
}

func (l *LidarStream) Stop() {
	if !l.running.Swap(false) {
		return
	}
	l.cancel()
	l.wg.Wait()
	close(l.done)
	slog.Info("lidar stream stopped")
}

func (l *LidarStream) loop(ctx context.Context) {
	defer l.wg.Done()
	for ctx.Err() == nil {
		if err := l.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("lidar recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (l *LidarStream) record(ctx context.Context) error {
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
		if _, err := fmt.Fprintln(tsFile, "unix_ns,seq,byte_offset,mono_ns"); err != nil {
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
		slog.Info("lidar resuming previous session",
			"starting_seq", seq,
			"starting_byte_offset", byteOffset)
	}

	keyExpr, err := zenoh.NewKeyExpr(l.cfg.ZenohTopic)
	if err != nil {
		return fmt.Errorf("create key expression: %w", err)
	}

	handler := zenoh.NewFifoChannel[zenoh.Sample](1024)

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

	slog.Info("lidar recorder started", "topic", l.cfg.ZenohTopic)

	receiver := subscriber.Handler()

	syncTicker := time.NewTicker(syncInterval)
	defer syncTicker.Stop()

	flush := func() {
		if err := dataFile.Sync(); err != nil {
			slog.Warn("lidar data sync failed", "err", err)
		}
		if err := tsFile.Sync(); err != nil {
			slog.Warn("lidar ts sync failed", "err", err)
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
			// Receive time on the boot clock. unixNs above is the publisher's
			// stamp and shares this host's wall clock, so a clock step spoils
			// both; monoNs is immune to the step and says which correction
			// applies to this row. See internal/clock.
			monoNs := clock.MonoNs()

			payload := sample.Payload()
			n, err := dataFile.Write(payload.Bytes())
			if err != nil {
				return fmt.Errorf("write data: %w", err)
			}

			if _, err := fmt.Fprintf(tsFile, "%d,%d,%d,%d\n", unixNs, seq, byteOffset, monoNs); err != nil {
				return fmt.Errorf("write timestamp: %w", err)
			}

			byteOffset += int64(n)
			seq++

			// Heartbeat tick. Safe if cfg.Monitor is nil (no-op).
			l.cfg.Monitor.Tick(HeartbeatName)
		}
	}
}
