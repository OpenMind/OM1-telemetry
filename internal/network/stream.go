package network

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"om1-telemetry/internal/clock"
	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/recordutil"
)

// HeartbeatName is the stream identifier for the heartbeat monitor.
const HeartbeatName = "network"

type Config struct {
	PingHost    string
	PingTimeout time.Duration

	PollInterval time.Duration

	DataFile string

	Monitor *heartbeat.Monitor
}

type NetworkStream struct {
	cfg     Config
	running atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}
	wg      sync.WaitGroup
}

func New(cfg Config) *NetworkStream {
	return &NetworkStream{cfg: cfg}
}

func (n *NetworkStream) Start() {
	if n.running.Swap(true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.done = make(chan struct{})
	n.wg.Add(1)
	go n.loop(ctx)
}

func (n *NetworkStream) Stop() {
	if !n.running.Swap(false) {
		return
	}
	n.cancel()
	n.wg.Wait()
	close(n.done)
	slog.Info("network stream stopped")
}

func (n *NetworkStream) loop(ctx context.Context) {
	defer n.wg.Done()
	for ctx.Err() == nil {
		// record() returns errors only for disk-write failures, not for ping
		// failures (those are recorded as reachable=false). So this loop
		// retries disk errors, not network errors.
		if err := n.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("network recorder error; retrying in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (n *NetworkStream) record(ctx context.Context) error {
	result, err := recordutil.OpenForAppend(n.cfg.DataFile)
	if err != nil {
		return fmt.Errorf("open data file: %w", err)
	}
	dataFile := result.File
	defer func() {
		if err := dataFile.Close(); err != nil {
			slog.Error("failed to close data file", "err", err)
		}
	}()

	if result.PrevSize == 0 {
		if _, err := fmt.Fprintln(dataFile, "unix_ns,seq,reachable,rtt_ms,packet_loss_pct,mono_ns"); err != nil {
			return fmt.Errorf("write header: %w", err)
		}
	}

	lastSeq, err := recordutil.ReadLastSeq(n.cfg.DataFile)
	if err != nil {
		slog.Warn("could not read last seq; starting from 0", "err", err)
		lastSeq = -1
	}
	seq := lastSeq + 1
	if result.PrevSize > 0 {
		slog.Info("network resuming previous session", "starting_seq", seq)
	}

	slog.Info("network recorder started", "host", n.cfg.PingHost, "interval", n.cfg.PollInterval)

	ticker := time.NewTicker(n.cfg.PollInterval)
	defer ticker.Stop()

	// Track whether we've already warned about a broken ping binary so we
	// don't spam the log every poll interval.
	pingBinaryWarned := false

	for {
		sample := n.ping(ctx, &pingBinaryWarned)
		if ctx.Err() != nil {
			return nil
		}
		if _, err := fmt.Fprintf(dataFile, "%d,%d,%t,%s,%s,%d\n",
			time.Now().UnixNano(), seq, sample.reachable,
			formatFloat(sample.rttMs, sample.reachable),
			formatFloat(sample.lossPct, true),
			clock.MonoNs(),
		); err != nil {
			return fmt.Errorf("write sample: %w", err)
		}
		if err := dataFile.Sync(); err != nil {
			return fmt.Errorf("sync data file: %w", err)
		}
		seq++

		n.cfg.Monitor.Tick(HeartbeatName)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type pingResult struct {
	reachable bool
	rttMs     float64
	lossPct   float64
}

// ping runs a single ping and parses the result. The binaryWarned pointer is
// used to log "ping binary missing" only once across the lifetime of the
// recorder, rather than every poll interval.
func (n *NetworkStream) ping(ctx context.Context, binaryWarned *bool) pingResult {
	timeoutSec := int(n.cfg.PingTimeout.Seconds())
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-W", strconv.Itoa(timeoutSec), n.cfg.PingHost)
	out, err := cmd.Output()
	if err != nil {
		// `ping` exiting non-zero (e.g., host unreachable) returns
		// *exec.ExitError — that's normal, we still want to parse output.
		// Anything else (binary missing, permission denied, etc.) is an
		// environment problem and we should surface it once.
		if _, isExit := err.(*exec.ExitError); !isExit {
			if !*binaryWarned {
				slog.Warn("network: ping command failed for non-exit reason "+
					"(check that `ping` is installed and PATH is correct)",
					"err", err, "host", n.cfg.PingHost)
				*binaryWarned = true
			}
		}
	} else if *binaryWarned {
		slog.Info("network: ping command working again")
		*binaryWarned = false
	}
	return parsePing(string(out))
}

var (
	lossRe = regexp.MustCompile(`([\d.]+)% packet loss`)
	rttRe  = regexp.MustCompile(`(?:rtt|round-trip).*?=\s*[\d.]+/([\d.]+)/`)
	timeRe = regexp.MustCompile(`time=([\d.]+)\s*ms`)
)

func parsePing(out string) pingResult {
	res := pingResult{lossPct: 100}

	if m := lossRe.FindStringSubmatch(out); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			res.lossPct = v
		}
	}

	if m := rttRe.FindStringSubmatch(out); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			res.rttMs = v
			res.reachable = true
		}
	} else if m := timeRe.FindStringSubmatch(out); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			res.rttMs = v
			res.reachable = true
		}
	}

	if res.lossPct >= 100 {
		res.reachable = false
	}
	return res
}

func formatFloat(v float64, valid bool) string {
	if !valid {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 3, 64)
}
