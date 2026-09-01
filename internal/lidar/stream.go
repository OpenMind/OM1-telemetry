package lidar

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
)

// HeartbeatName is the stream identifier used with heartbeat.Monitor.
// Register it in main.go as: mon.Register(lidar.HeartbeatName, 10)
const HeartbeatName = "lidar"

const syncInterval = 2 * time.Second

type Config struct {
	DDSDomainID    uint32
	DDSTopic       string
	TimestampsFile string
	DataFile       string

	Monitor *heartbeat.Monitor
}

type LidarStream struct {
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

func New(cfg Config) *LidarStream {
	return &LidarStream{cfg: cfg, reconnect: make(chan struct{}, 1)}
}

func (l *LidarStream) Start() {
	if l.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.wg.Add(1)
	go l.loop(ctx)
}

func (l *LidarStream) Stop() {
	if !l.running.Swap(false) {
		return
	}
	l.cancel()
	l.wg.Wait()
	l.closeFiles()
	slog.Info("lidar stream stopped")
}

// Rotate switches the stream's output to a new pair of files without
// touching the DDS subscription, so a session rotation never has to
// resubscribe -- and so never drops samples the way a Stop+Start cycle would.
func (l *LidarStream) Rotate(dataFile, timestampsFile string) error {
	files, err := openOutputFiles(dataFile, timestampsFile)
	if err != nil {
		return fmt.Errorf("rotate: %w", err)
	}

	l.filesMu.Lock()
	old := l.files
	l.files = files
	l.filesMu.Unlock()

	if old != nil {
		closeOutputFiles(old, "lidar")
	}
	return nil
}

// Reconnect tears down and recreates the DDS subscription without touching
// output files or the dedup/rotation state. Plain DDS discovery does not
// always self-heal after the publisher restarts (see heartbeat.Monitor's
// RegisterRecoverable doc comment for why); this forces a fresh discovery
// attempt on demand. Non-blocking -- a request that arrives while one is
// already pending is dropped, since it would have the same effect.
func (l *LidarStream) Reconnect() {
	select {
	case l.reconnect <- struct{}{}:
	default:
	}
}

func (l *LidarStream) loop(ctx context.Context) {
	defer l.wg.Done()
	for ctx.Err() == nil {
		if err := l.ensureFilesOpen(); err != nil {
			slog.Error("lidar: cannot open output files; retrying in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if err := l.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("lidar recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// ensureFilesOpen opens the stream's initial output files, unless a Rotate
// call already installed a pair first.
func (l *LidarStream) ensureFilesOpen() error {
	l.filesMu.Lock()
	defer l.filesMu.Unlock()
	if l.files != nil {
		return nil
	}
	files, err := openOutputFiles(l.cfg.DataFile, l.cfg.TimestampsFile)
	if err != nil {
		return err
	}
	l.files = files
	return nil
}

func (l *LidarStream) record(ctx context.Context) error {
	receiver, closeSub, err := subscribeDDS(ctx, l.cfg.DDSDomainID, l.cfg.DDSTopic)
	if err != nil {
		return fmt.Errorf("subscribe dds: %w", err)
	}
	defer closeSub()

	slog.Info("lidar recorder started", "domain", l.cfg.DDSDomainID, "topic", l.cfg.DDSTopic)

	syncTicker := time.NewTicker(syncInterval)
	defer syncTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			l.flush()
			return nil
		case <-l.reconnect:
			l.flush()
			return fmt.Errorf("reconnect requested")
		case <-syncTicker.C:
			l.flush()
		case sample, ok := <-receiver:
			if !ok {
				l.flush()
				return fmt.Errorf("dds subscriber channel closed")
			}

			unixNs := sample.unixNs
			if unixNs == 0 {
				unixNs = time.Now().UnixNano()
			}
			// monoNs pairs with unixNs so a later clock correction can be
			// reapplied; unaffected by wall-clock steps. See internal/clock.
			monoNs := clock.MonoNs()

			if err := l.write(sample.data, unixNs, monoNs); err != nil {
				return err
			}

			l.cfg.Monitor.Tick(HeartbeatName)
		}
	}
}

// write appends one sample to the current output files. Held under filesMu
// for the whole write so a concurrent Rotate can never split a sample
// across the old and new files.
func (l *LidarStream) write(data []byte, unixNs, monoNs int64) error {
	l.filesMu.Lock()
	defer l.filesMu.Unlock()
	f := l.files

	n, err := f.data.Write(data)
	if err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	if _, err := fmt.Fprintf(f.ts, "%d,%d,%d,%d\n", unixNs, f.seq, f.byteOffset, monoNs); err != nil {
		return fmt.Errorf("write timestamp: %w", err)
	}
	f.byteOffset += int64(n)
	f.seq++
	return nil
}

func (l *LidarStream) flush() {
	l.filesMu.Lock()
	f := l.files
	l.filesMu.Unlock()
	if f == nil {
		return
	}
	if err := f.data.Sync(); err != nil {
		slog.Warn("lidar data sync failed", "err", err)
	}
	if err := f.ts.Sync(); err != nil {
		slog.Warn("lidar ts sync failed", "err", err)
	}
}

func (l *LidarStream) closeFiles() {
	l.filesMu.Lock()
	f := l.files
	l.files = nil
	l.filesMu.Unlock()
	if f != nil {
		closeOutputFiles(f, "lidar")
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
		if _, err := fmt.Fprintln(tsResult.File, "unix_ns,seq,byte_offset,mono_ns"); err != nil {
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
		slog.Info("lidar resuming previous session",
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
