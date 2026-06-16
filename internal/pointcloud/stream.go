package pointcloud

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse-zenoh/zenoh-go/zenoh"
	"github.com/klauspost/compress/zstd"

	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
	"om1-telemetry/internal/zenohutil"
)

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
const HeartbeatName = "pointcloud"

const syncInterval = 2 * time.Second

type Config struct {
	ZenohEndpoint  string
	ZenohTopic     string
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

type SessionResult struct {
	session zenoh.Session
	err     error
}

type SubscriberResult struct {
	subscriber zenoh.Subscriber
	err        error
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
	config := zenoh.NewConfigDefault()
	if err := config.InsertJson5(zenoh.ConfigModeKey, `"client"`); err != nil {
		return err
	}
	endpoint := c.cfg.ZenohEndpoint
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
			"unix_ns,seq,byte_offset,byte_length,method"); err != nil {
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

	keyExpr, err := zenoh.NewKeyExpr(c.cfg.ZenohTopic)
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

	slog.Info("pointcloud recorder started", "topic", c.cfg.ZenohTopic)

	receiver := subscriber.Handler()

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

			data, method := encodeFrame(encoder, sample.Payload().Bytes())

			n, err := dataFile.Write(data)
			if err != nil {
				return fmt.Errorf("write data: %w", err)
			}

			if _, err := fmt.Fprintf(tsFile, "%d,%d,%d,%d,%s\n",
				unixNs, seq, byteOffset, n, method); err != nil {
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
