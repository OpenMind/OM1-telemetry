package pointcloud

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"om1-telemetry/internal/clock"
	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
)

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
const HeartbeatName = "pointcloud"

const syncInterval = 2 * time.Second

type Config struct {
	DDSDomainID    uint32
	DDSTopic       string
	TimestampsFile string
	DataFile       string

	Monitor *heartbeat.Monitor
}

type PointCloudStream struct {
	cfg     Config
	running atomic.Bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// encoder is only ever touched by the loop goroutine (created lazily
	// there, read there, and only read back in Stop after wg.Wait), so it
	// needs no lock of its own.
	encoder *zstd.Encoder

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

func New(cfg Config) *PointCloudStream {
	return &PointCloudStream{cfg: cfg}
}

func (c *PointCloudStream) Start() {
	if c.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.wg.Add(1)
	go c.loop(ctx)
}

func (c *PointCloudStream) Stop() {
	if !c.running.Swap(false) {
		return
	}
	c.cancel()
	c.wg.Wait()
	c.closeFiles()
	if c.encoder != nil {
		if err := c.encoder.Close(); err != nil {
			slog.Error("failed to close zstd encoder", "err", err)
		}
		c.encoder = nil
	}
	slog.Info("pointcloud stream stopped")
}

// Rotate switches the stream's output to a new pair of files without
// touching the DDS subscription, so a session rotation never has to
// resubscribe -- and so never drops samples the way a Stop+Start cycle would.
func (c *PointCloudStream) Rotate(dataFile, timestampsFile string) error {
	files, err := openOutputFiles(dataFile, timestampsFile)
	if err != nil {
		return fmt.Errorf("rotate: %w", err)
	}

	c.filesMu.Lock()
	old := c.files
	c.files = files
	c.filesMu.Unlock()

	if old != nil {
		closeOutputFiles(old, "pointcloud")
	}
	return nil
}

func (c *PointCloudStream) loop(ctx context.Context) {
	defer c.wg.Done()
	for ctx.Err() == nil {
		if err := c.ensureReady(); err != nil {
			slog.Error("pointcloud: cannot initialize; retrying in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if err := c.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("pointcloud recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// ensureReady opens the stream's initial output files (unless a Rotate call
// already installed a pair first) and lazily creates the zstd encoder.
func (c *PointCloudStream) ensureReady() error {
	if err := c.ensureFilesOpen(); err != nil {
		return err
	}
	if c.encoder == nil {
		enc, err := zstd.NewWriter(nil)
		if err != nil {
			return fmt.Errorf("create zstd encoder: %w", err)
		}
		c.encoder = enc
	}
	return nil
}

func (c *PointCloudStream) ensureFilesOpen() error {
	c.filesMu.Lock()
	defer c.filesMu.Unlock()
	if c.files != nil {
		return nil
	}
	files, err := openOutputFiles(c.cfg.DataFile, c.cfg.TimestampsFile)
	if err != nil {
		return err
	}
	c.files = files
	return nil
}

func (c *PointCloudStream) record(ctx context.Context) error {
	receiver, closeSub, err := subscribeDDS(ctx, c.cfg.DDSDomainID, c.cfg.DDSTopic)
	if err != nil {
		return fmt.Errorf("subscribe dds: %w", err)
	}
	defer closeSub()

	slog.Info("pointcloud recorder started", "domain", c.cfg.DDSDomainID, "topic", c.cfg.DDSTopic)

	syncTicker := time.NewTicker(syncInterval)
	defer syncTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.flush()
			return nil
		case <-syncTicker.C:
			c.flush()
		case sample, ok := <-receiver:
			if !ok {
				c.flush()
				return fmt.Errorf("dds subscriber channel closed")
			}

			unixNs := sample.unixNs
			if unixNs == 0 {
				unixNs = time.Now().UnixNano()
			}
			// monoNs pairs with unixNs so a later clock correction can be
			// reapplied; unaffected by wall-clock steps. See internal/clock.
			monoNs := clock.MonoNs()

			if err := c.write(sample.data, unixNs, monoNs); err != nil {
				return err
			}

			c.cfg.Monitor.Tick(HeartbeatName)
		}
	}
}

// write encodes and appends one frame to the current output files. The file
// write is held under filesMu so a concurrent Rotate can never split a frame
// across the old and new files.
func (c *PointCloudStream) write(raw []byte, unixNs, monoNs int64) error {
	data, method := encodeFrame(c.encoder, raw)

	c.filesMu.Lock()
	defer c.filesMu.Unlock()
	f := c.files

	n, err := f.data.Write(data)
	if err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	if _, err := fmt.Fprintf(f.ts, "%d,%d,%d,%d,%s,%d\n",
		unixNs, f.seq, f.byteOffset, n, method, monoNs); err != nil {
		return fmt.Errorf("write timestamp: %w", err)
	}
	f.byteOffset += int64(n)
	f.seq++
	return nil
}

func (c *PointCloudStream) flush() {
	c.filesMu.Lock()
	f := c.files
	c.filesMu.Unlock()
	if f == nil {
		return
	}
	if err := f.data.Sync(); err != nil {
		slog.Warn("pointcloud data sync failed", "err", err)
	}
	if err := f.ts.Sync(); err != nil {
		slog.Warn("pointcloud ts sync failed", "err", err)
	}
}

func (c *PointCloudStream) closeFiles() {
	c.filesMu.Lock()
	f := c.files
	c.files = nil
	c.filesMu.Unlock()
	if f != nil {
		closeOutputFiles(f, "pointcloud")
	}
}

func encodeFrame(encoder *zstd.Encoder, raw []byte) (data []byte, method string) {
	compressed := encoder.EncodeAll(raw, make([]byte, 0, len(raw)))
	if len(compressed) >= len(raw) {
		return raw, "raw"
	}
	return compressed, "zstd"
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
			"unix_ns,seq,byte_offset,byte_length,method,mono_ns"); err != nil {
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
		slog.Info("pointcloud resuming previous session",
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
