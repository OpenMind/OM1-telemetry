package depth

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
	"om1-telemetry/internal/rvl"
	"om1-telemetry/internal/zenohutil"
)

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
const HeartbeatName = "depth"

const syncInterval = 2 * time.Second

type Config struct {
	ZenohEndpoint  string
	ZenohTopic     string
	TimestampsFile string
	DataFile       string

	// Monitor is optional; ticks once per stored frame so the central
	// heartbeat monitor can detect a stuck recorder.
	Monitor *heartbeat.Monitor
}

type DepthStream struct {
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

func New(cfg Config) *DepthStream {
	return &DepthStream{cfg: cfg}
}

func (d *DepthStream) Start() {
	if d.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.done = make(chan struct{})
	d.wg.Add(1)
	go d.loop(ctx)
}

func (d *DepthStream) Stop() {
	if !d.running.Swap(false) {
		return
	}
	d.cancel()
	d.wg.Wait()
	close(d.done)
	slog.Info("depth stream stopped")
}

func (d *DepthStream) loop(ctx context.Context) {
	defer d.wg.Done()
	for ctx.Err() == nil {
		if err := d.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("depth recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (d *DepthStream) record(ctx context.Context) error {
	config := zenoh.NewConfigDefault()
	if err := config.InsertJson5(zenoh.ConfigModeKey, `"client"`); err != nil {
		return err
	}
	endpoint := d.cfg.ZenohEndpoint
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
	tsResult, err := recordutil.OpenForAppend(d.cfg.TimestampsFile)
	if err != nil {
		return fmt.Errorf("open timestamps file: %w", err)
	}
	tsFile := tsResult.File
	defer func() {
		if err := tsFile.Close(); err != nil {
			slog.Error("failed to close timestamps file", "err", err)
		}
	}()

	dataResult, err := recordutil.OpenForAppend(d.cfg.DataFile)
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
		if _, err := fmt.Fprintln(tsFile,
			"unix_ns,seq,byte_offset,byte_length,method,width,height,encoding"); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
	}

	lastSeq, err := recordutil.ReadLastSeq(d.cfg.TimestampsFile)
	if err != nil {
		slog.Warn("could not read last seq; starting from 0", "err", err)
		lastSeq = -1
	}
	seq := lastSeq + 1
	byteOffset := dataResult.PrevSize

	if dataResult.PrevSize > 0 {
		slog.Info("depth resuming previous session",
			"starting_seq", seq,
			"starting_byte_offset", byteOffset)
	}

	keyExpr, err := zenoh.NewKeyExpr(d.cfg.ZenohTopic)
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

	slog.Info("depth recorder started", "topic", d.cfg.ZenohTopic)

	receiver := subscriber.Handler()

	syncTicker := time.NewTicker(syncInterval)
	defer syncTicker.Stop()

	flush := func() {
		if err := dataFile.Sync(); err != nil {
			slog.Warn("depth data sync failed", "err", err)
		}
		if err := tsFile.Sync(); err != nil {
			slog.Warn("depth ts sync failed", "err", err)
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

			payload := sample.Payload().Bytes()
			f := encodeFrame(payload)

			n, err := dataFile.Write(f.data)
			if err != nil {
				return fmt.Errorf("write data: %w", err)
			}

			if _, err := fmt.Fprintf(tsFile, "%d,%d,%d,%d,%s,%d,%d,%s\n",
				unixNs, seq, byteOffset, n, f.method, f.width, f.height, f.encoding); err != nil {
				return fmt.Errorf("write timestamp: %w", err)
			}

			byteOffset += int64(n)
			seq++

			// Heartbeat tick. Safe if Monitor is nil.
			d.cfg.Monitor.Tick(HeartbeatName)
		}
	}
}

type frame struct {
	data     []byte
	method   string // "rvl" (compressed) or "raw" (passthrough)
	width    uint32
	height   uint32
	encoding string
}

func encodeFrame(payload []byte) frame {
	img, err := ParseImage(payload)
	if err != nil {
		slog.Warn("depth: cannot parse image, storing raw", "err", err)
		return frame{data: payload, method: "raw"}
	}

	// Defensive check: if step has row padding (step != width*2), the
	// DepthPixels reader would misalign rows. Fall back to raw — the
	// data is preserved as-is, downstream decoders can handle it.
	if img.Step != img.Width*2 {
		slog.Warn("depth: step has padding, storing raw",
			"step", img.Step, "width_times_2", img.Width*2)
		return frame{
			data: payload, method: "raw",
			width: img.Width, height: img.Height, encoding: img.Encoding,
		}
	}

	pixels, err := img.DepthPixels()
	if err != nil {
		slog.Warn("depth: not a 16-bit depth frame, storing raw", "err", err, "encoding", img.Encoding)
		return frame{data: payload, method: "raw", encoding: img.Encoding}
	}

	return frame{
		data:     rvl.Encode(pixels),
		method:   "rvl",
		width:    img.Width,
		height:   img.Height,
		encoding: img.Encoding,
	}
}
