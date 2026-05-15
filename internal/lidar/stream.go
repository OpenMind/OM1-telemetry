package lidar

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse-zenoh/zenoh-go/zenoh"
)

type Config struct {
	ZenohTopic     string
	TimestampsFile string
	DataFile       string
}

type LidarStream struct {
	cfg     Config
	running atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}
	wg      sync.WaitGroup
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
	session, err := zenoh.Open(config, nil)
	if err != nil {
		return fmt.Errorf("open zenoh session: %w", err)
	}
	defer session.Drop()

	tsFile, err := os.Create(l.cfg.TimestampsFile)
	if err != nil {
		return fmt.Errorf("create timestamps file: %w", err)
	}
	defer func() {
		if err := tsFile.Close(); err != nil {
			slog.Error("failed to close timestamps file", "err", err)
		}
	}()

	dataFile, err := os.Create(l.cfg.DataFile)
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

	keyExpr, err := zenoh.NewKeyExpr(l.cfg.ZenohTopic)
	if err != nil {
		return fmt.Errorf("create key expression: %w", err)
	}

	handler := zenoh.NewFifoChannel[zenoh.Sample](1024)
	subscriber, err := session.DeclareSubscriber(keyExpr, handler, nil)
	if err != nil {
		return fmt.Errorf("declare subscriber: %w", err)
	}
	defer subscriber.Drop()

	slog.Info("lidar recorder started", "topic", l.cfg.ZenohTopic)

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
				unixNs = zenohTimestampToUnixNs(ts)
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

func zenohTimestampToUnixNs(ts zenoh.TimeStamp) int64 {
	return ntpTimeToUnixNs(ts.Time())
}

func ntpTimeToUnixNs(ntpTime uint64) int64 {
	const ntpToUnixOffset = 2208988800

	seconds := int64(ntpTime >> 32)
	fraction := uint32(ntpTime & 0xFFFFFFFF)

	unixSeconds := seconds - ntpToUnixOffset

	nanos := (int64(fraction) * 1e9) >> 32

	return unixSeconds*1e9 + nanos
}
