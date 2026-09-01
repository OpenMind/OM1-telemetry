package odom

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

const HeartbeatName = "odom"

const syncInterval = 2 * time.Second

type Config struct {
	DDSDomainID    uint32
	DDSTopic       string
	TimestampsFile string
	DataFile       string

	Monitor *heartbeat.Monitor
}

type OdomStream struct {
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

func New(cfg Config) *OdomStream {
	return &OdomStream{cfg: cfg, reconnect: make(chan struct{}, 1)}
}

func (o *OdomStream) Start() {
	if o.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	o.cancel = cancel
	o.wg.Add(1)
	go o.loop(ctx)
}

func (o *OdomStream) Stop() {
	if !o.running.Swap(false) {
		return
	}
	o.cancel()
	o.wg.Wait()
	o.closeFiles()
	slog.Info("odom stream stopped")
}

// Rotate switches the stream's output to a new pair of files without
// touching the DDS subscription, so a session rotation never has to
// resubscribe -- and so never drops samples the way a Stop+Start cycle would.
func (o *OdomStream) Rotate(dataFile, timestampsFile string) error {
	files, err := openOutputFiles(dataFile, timestampsFile)
	if err != nil {
		return fmt.Errorf("rotate: %w", err)
	}

	o.filesMu.Lock()
	old := o.files
	o.files = files
	o.filesMu.Unlock()

	if old != nil {
		closeOutputFiles(old, "odom")
	}
	return nil
}

// Reconnect tears down and recreates the DDS subscription without touching
// output files. Plain DDS discovery does not always self-heal after the
// publisher restarts (see heartbeat.Monitor's RegisterRecoverable doc
// comment for why); this forces a fresh discovery attempt on demand.
// Non-blocking -- a request that arrives while one is already pending is
// dropped, since it would have the same effect.
func (o *OdomStream) Reconnect() {
	select {
	case o.reconnect <- struct{}{}:
	default:
	}
}

func (o *OdomStream) loop(ctx context.Context) {
	defer o.wg.Done()
	for ctx.Err() == nil {
		if err := o.ensureFilesOpen(); err != nil {
			slog.Error("odom: cannot open output files; retrying in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if err := o.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("odom recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// ensureFilesOpen opens the stream's initial output files, unless a Rotate
// call already installed a pair first.
func (o *OdomStream) ensureFilesOpen() error {
	o.filesMu.Lock()
	defer o.filesMu.Unlock()
	if o.files != nil {
		return nil
	}
	files, err := openOutputFiles(o.cfg.DataFile, o.cfg.TimestampsFile)
	if err != nil {
		return err
	}
	o.files = files
	return nil
}

func (o *OdomStream) record(ctx context.Context) error {
	receiver, closeSub, err := subscribeDDS(ctx, o.cfg.DDSDomainID, o.cfg.DDSTopic)
	if err != nil {
		return fmt.Errorf("subscribe dds: %w", err)
	}
	defer closeSub()

	slog.Info("odom recorder started", "domain", o.cfg.DDSDomainID, "topic", o.cfg.DDSTopic)

	syncTicker := time.NewTicker(syncInterval)
	defer syncTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			o.flush()
			return nil
		case <-o.reconnect:
			o.flush()
			return fmt.Errorf("reconnect requested")
		case <-syncTicker.C:
			o.flush()
		case sample, ok := <-receiver:
			if !ok {
				o.flush()
				return fmt.Errorf("dds subscriber channel closed")
			}

			unixNs := sample.unixNs
			if unixNs == 0 {
				unixNs = time.Now().UnixNano()
			}
			// monoNs pairs with unixNs so a later clock correction can be
			// reapplied; unaffected by wall-clock steps. See internal/clock.
			monoNs := clock.MonoNs()

			if err := o.write(sample.data, unixNs, monoNs); err != nil {
				return err
			}

			o.cfg.Monitor.Tick(HeartbeatName)
		}
	}
}

// write appends one sample to the current output files. Held under filesMu
// for the whole write so a concurrent Rotate can never split a sample
// across the old and new files.
func (o *OdomStream) write(data []byte, unixNs, monoNs int64) error {
	o.filesMu.Lock()
	defer o.filesMu.Unlock()
	f := o.files

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

func (o *OdomStream) flush() {
	o.filesMu.Lock()
	f := o.files
	o.filesMu.Unlock()
	if f == nil {
		return
	}
	if err := f.data.Sync(); err != nil {
		slog.Warn("odom data sync failed", "err", err)
	}
	if err := f.ts.Sync(); err != nil {
		slog.Warn("odom ts sync failed", "err", err)
	}
}

func (o *OdomStream) closeFiles() {
	o.filesMu.Lock()
	f := o.files
	o.files = nil
	o.filesMu.Unlock()
	if f != nil {
		closeOutputFiles(f, "odom")
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
		slog.Info("odom resuming previous session",
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
