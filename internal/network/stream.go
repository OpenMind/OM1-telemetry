package network

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	PingHost    string
	PingTimeout time.Duration

	PollInterval time.Duration

	DataFile string
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
		if err := n.record(ctx); err != nil && ctx.Err() == nil {
			slog.Error("network recorder error; reconnecting in 2 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (n *NetworkStream) record(ctx context.Context) error {
	dataFile, err := os.Create(n.cfg.DataFile)
	if err != nil {
		return fmt.Errorf("create data file: %w", err)
	}
	defer func() {
		if err := dataFile.Close(); err != nil {
			slog.Error("failed to close data file", "err", err)
		}
	}()

	if _, err := fmt.Fprintln(dataFile, "unix_ns,seq,reachable,rtt_ms,packet_loss_pct"); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	slog.Info("network recorder started", "host", n.cfg.PingHost, "interval", n.cfg.PollInterval)

	ticker := time.NewTicker(n.cfg.PollInterval)
	defer ticker.Stop()

	var seq int64
	for {
		sample := n.ping(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if _, err := fmt.Fprintf(dataFile, "%d,%d,%t,%s,%s\n",
			time.Now().UnixNano(), seq, sample.reachable,
			formatFloat(sample.rttMs, sample.reachable),
			formatFloat(sample.lossPct, true),
		); err != nil {
			return fmt.Errorf("write sample: %w", err)
		}
		if err := dataFile.Sync(); err != nil {
			return fmt.Errorf("sync data file: %w", err)
		}
		seq++

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

func (n *NetworkStream) ping(ctx context.Context) pingResult {
	timeoutSec := int(n.cfg.PingTimeout.Seconds())
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-W", strconv.Itoa(timeoutSec), n.cfg.PingHost)
	out, _ := cmd.Output()
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
