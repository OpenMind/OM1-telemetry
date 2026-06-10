package pointcloud

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse-zenoh/zenoh-go/zenoh"

	"om1-telemetry/internal/zenohutil"
)

type Config struct {
	ZenohEndpoint  string
	ZenohTopic     string
	TimestampsFile string
	DataFile       string
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

	tsFile, err := os.Create(c.cfg.TimestampsFile)
	if err != nil {
		return fmt.Errorf("create timestamps file: %w", err)
	}
	defer func() {
		if err := tsFile.Close(); err != nil {
			slog.Error("failed to close timestamps file", "err", err)
		}
	}()

	dataFile, err := os.Create(c.cfg.DataFile)
	if err != nil {
		return fmt.Errorf("create data file: %w", err)
	}
	defer func() {
		if err := dataFile.Close(); err != nil {
			slog.Error("failed to close data file", "err", err)
		}
	}()

	if _, err := fmt.Fprintln(tsFile, "unix_ns,seq,byte_offset"); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

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

	var seq int64
	var byteOffset int64
	receiver := subscriber.Handler()

	for {
		select {
		case <-ctx.Done():
			return nil
		case sample, ok := <-receiver:
			if !ok {
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
		}
	}
}
