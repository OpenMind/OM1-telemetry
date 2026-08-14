package odom

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
const HeartbeatName = "odom"

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

type OdomStream struct {
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
	config := zenoh.NewConfigDefault()
	if err := config.InsertJson5(zenoh.ConfigModeKey, `"client"`); err != nil {
		return err
	}
	endpoint := o.cfg.ZenohEndpoint
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

	keyExpr, err := zenoh.NewKeyExpr(o.cfg.ZenohTopic)
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

	slog.Info("odom recorder started", "topic", o.cfg.ZenohTopic)

	receiver := subscriber.Handler()

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

			// Heartbeat tick: safe if Monitor is nil.
			o.cfg.Monitor.Tick(HeartbeatName)
		}
	}
}
