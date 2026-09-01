package depth

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"om1-telemetry/internal/clock"
	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
	"om1-telemetry/internal/rvl"
)

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
const HeartbeatName = "depth"

const syncInterval = 2 * time.Second

type Config struct {
	DDSDomainID    uint32
	DDSTopic       string
	TimestampsFile string
	DataFile       string

	Monitor *heartbeat.Monitor
}

type DepthStream struct {
	cfg     Config
	running atomic.Bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// reconnect asks a running record() to tear down and recreate its DDS
	// subscription. Buffered so a request never blocks; see Reconnect.
	reconnect chan struct{}

	filesMu sync.Mutex
	files   *outputFiles
}

// outputFiles is the current pair of open output files plus the counters
// that continue across a Rotate.
type outputFiles struct {
	data       *os.File
	ts         *os.File
	seq        int64
	byteOffset int64
}

func New(cfg Config) *DepthStream {
	return &DepthStream{cfg: cfg, reconnect: make(chan struct{}, 1)}
}

func (d *DepthStream) Start() {
	if d.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.wg.Add(1)
	go d.loop(ctx)
}

func (d *DepthStream) Stop() {
	if !d.running.Swap(false) {
		return
	}
	d.cancel()
	d.wg.Wait()
	d.closeFiles()
	slog.Info("depth stream stopped")
}

// Rotate switches the stream's output to a new pair of files without
// touching the DDS subscription, so a session rotation never has to
// resubscribe -- and so never drops samples the way a Stop+Start cycle would.
func (d *DepthStream) Rotate(dataFile, timestampsFile string) error {
	files, err := openOutputFiles(dataFile, timestampsFile)
	if err != nil {
		return fmt.Errorf("rotate: %w", err)
	}

	d.filesMu.Lock()
	old := d.files
	d.files = files
	d.filesMu.Unlock()

	if old != nil {
		closeOutputFiles(old, "depth")
	}
	return nil
}

// Reconnect tears down and recreates the DDS subscription without touching
// output files. Non-blocking; see heartbeat.Monitor.RegisterRecoverable.
func (d *DepthStream) Reconnect() {
	select {
	case d.reconnect <- struct{}{}:
	default:
	}
}

func (d *DepthStream) loop(ctx context.Context) {
	defer d.wg.Done()
	for ctx.Err() == nil {
		if err := d.ensureFilesOpen(); err != nil {
			slog.Error("depth: cannot open output files; retrying in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if err := d.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("depth recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// ensureFilesOpen opens the stream's initial output files, unless a Rotate
// call already installed a pair first.
func (d *DepthStream) ensureFilesOpen() error {
	d.filesMu.Lock()
	defer d.filesMu.Unlock()
	if d.files != nil {
		return nil
	}
	files, err := openOutputFiles(d.cfg.DataFile, d.cfg.TimestampsFile)
	if err != nil {
		return err
	}
	d.files = files
	return nil
}

func (d *DepthStream) record(ctx context.Context) error {
	receiver, closeSub, err := subscribeDDS(ctx, d.cfg.DDSDomainID, d.cfg.DDSTopic)
	if err != nil {
		return fmt.Errorf("subscribe dds: %w", err)
	}
	defer closeSub()

	slog.Info("depth recorder started", "domain", d.cfg.DDSDomainID, "topic", d.cfg.DDSTopic)

	syncTicker := time.NewTicker(syncInterval)
	defer syncTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.flush()
			return nil
		case <-d.reconnect:
			d.flush()
			return fmt.Errorf("reconnect requested")
		case <-syncTicker.C:
			d.flush()
		case sample, ok := <-receiver:
			if !ok {
				d.flush()
				return fmt.Errorf("dds subscriber channel closed")
			}

			unixNs := sample.unixNs
			if unixNs == 0 {
				unixNs = time.Now().UnixNano()
			}
			// monoNs pairs with unixNs so a later clock correction can be
			// reapplied; unaffected by wall-clock steps. See internal/clock.
			monoNs := clock.MonoNs()

			if err := d.write(sample.data, unixNs, monoNs); err != nil {
				return err
			}

			d.cfg.Monitor.Tick(HeartbeatName)
		}
	}
}

// write encodes and appends one frame to the current output files. The file
// write is held under filesMu so a concurrent Rotate can never split a frame
// across the old and new files.
func (d *DepthStream) write(raw []byte, unixNs, monoNs int64) error {
	f := encodeFrame(raw)

	d.filesMu.Lock()
	defer d.filesMu.Unlock()
	out := d.files

	n, err := out.data.Write(f.data)
	if err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	if _, err := fmt.Fprintf(out.ts, "%d,%d,%d,%d,%s,%d,%d,%s,%d\n",
		unixNs, out.seq, out.byteOffset, n, f.method, f.width, f.height, f.encoding, monoNs); err != nil {
		return fmt.Errorf("write timestamp: %w", err)
	}
	out.byteOffset += int64(n)
	out.seq++
	return nil
}

func (d *DepthStream) flush() {
	d.filesMu.Lock()
	f := d.files
	d.filesMu.Unlock()
	if f == nil {
		return
	}
	if err := f.data.Sync(); err != nil {
		slog.Warn("depth data sync failed", "err", err)
	}
	if err := f.ts.Sync(); err != nil {
		slog.Warn("depth ts sync failed", "err", err)
	}
}

func (d *DepthStream) closeFiles() {
	d.filesMu.Lock()
	f := d.files
	d.files = nil
	d.filesMu.Unlock()
	if f != nil {
		closeOutputFiles(f, "depth")
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

// openOutputFiles opens dataPath/tsPath in append mode and resumes their
// counters from what's already on disk, so a reconnect or a process restart
// never clobbers or duplicates prior data.
func openOutputFiles(dataPath, tsPath string) (*outputFiles, error) {
	tsResult, err := recordutil.OpenForAppend(tsPath)
	if err != nil {
		return nil, fmt.Errorf("open timestamps file: %w", err)
	}
	dataResult, err := recordutil.OpenForAppend(dataPath)
	if err != nil {
		_ = tsResult.File.Close()
		return nil, fmt.Errorf("open data file: %w", err)
	}

	if tsResult.PrevSize == 0 {
		if _, err := fmt.Fprintln(tsResult.File,
			"unix_ns,seq,byte_offset,byte_length,method,width,height,encoding,mono_ns"); err != nil {
			_ = tsResult.File.Close()
			_ = dataResult.File.Close()
			return nil, fmt.Errorf("write header: %w", err)
		}
	}

	lastSeq, err := recordutil.ReadLastSeq(tsPath)
	if err != nil {
		slog.Warn("could not read last seq; starting from 0", "err", err)
		lastSeq = -1
	}

	if dataResult.PrevSize > 0 {
		slog.Info("depth resuming previous session",
			"starting_seq", lastSeq+1,
			"starting_byte_offset", dataResult.PrevSize)
	}

	return &outputFiles{
		data:       dataResult.File,
		ts:         tsResult.File,
		seq:        lastSeq + 1,
		byteOffset: dataResult.PrevSize,
	}, nil
}

func closeOutputFiles(f *outputFiles, streamName string) {
	if err := f.data.Sync(); err != nil {
		slog.Warn(streamName+" data sync failed", "err", err)
	}
	if err := f.data.Close(); err != nil {
		slog.Error("failed to close data file", "err", err)
	}
	if err := f.ts.Sync(); err != nil {
		slog.Warn(streamName+" ts sync failed", "err", err)
	}
	if err := f.ts.Close(); err != nil {
		slog.Error("failed to close timestamps file", "err", err)
	}
}
