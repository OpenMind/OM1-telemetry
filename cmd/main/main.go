package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"om1-telemetry/config"
	"om1-telemetry/internal/audio"
	"om1-telemetry/internal/clock"
	"om1-telemetry/internal/control"
	"om1-telemetry/internal/depth"
	"om1-telemetry/internal/features"
	"om1-telemetry/internal/heartbeat"
	"om1-telemetry/internal/lidar"
	"om1-telemetry/internal/lowstate"
	"om1-telemetry/internal/network"
	"om1-telemetry/internal/odom"
	"om1-telemetry/internal/pointcloud"
	"om1-telemetry/internal/session"
	"om1-telemetry/internal/upload"
	"om1-telemetry/internal/video"
)

// uploadTimeout bounds one session's upload, whether kicked off by rotation or shutdown.
const uploadTimeout = 10 * time.Minute

func main() {
	// Sampled before anything else, so the session's monotonic anchor covers
	// the whole run.
	clk := clock.New()

	recordingsDir := config.RecordingsDir()

	sess, err := session.Open(recordingsDir, clk)
	if err != nil {
		slog.Error("cannot create session directory", "dir", recordingsDir, "err", err)
		os.Exit(1)
	}

	// Sweep sessions an earlier run left undated. After Open, so this run's own
	// pending directory is excluded.
	session.Janitor(recordingsDir, sess.RealDir())

	cfg := config.Load(sess.Dir())
	sess.SetRobotType(string(cfg.RobotType))

	// Captured once: cfg itself is reassigned by the event loop below, and
	// reading cfg.Retention from another goroutine later would race.
	maxBytes := cfg.Retention.MaxBytes

	if !cfg.Collect {
		slog.Info("data collection disabled via ENABLE_COLLECTION=false, exiting")
		return
	}

	videoHeartbeatNames := make([]string, len(cfg.Video))
	for i, vc := range cfg.Video {
		videoHeartbeatNames[i] = "video_" + vc.Name
	}

	mon := heartbeat.NewMonitor(30 * time.Second)
	registerHeartbeats(mon, cfg, videoHeartbeatNames)

	hbCtx, hbCancel := context.WithCancel(context.Background())
	go mon.Run(hbCtx)

	var currentMu sync.Mutex
	current := sess

	bootTimebasePath := sess.TimebasePath()

	// Journals the monotonic-to-UTC mapping and, the first time NTP confirms
	// the wall clock, moves a pending session to its true date. Recorders write
	// through a symlink, so the rename does not disturb them.
	clkCtx, clkCancel := context.WithCancel(context.Background())
	watcher := clock.NewWatcher(clk, bootTimebasePath, func(clock.Record) {
		currentMu.Lock()
		s := current
		currentMu.Unlock()
		if err := s.Promote(); err != nil {
			slog.Error("could not date the session", "err", err)
		}
	})
	go watcher.Run(clkCtx)

	var uploader *upload.Client
	if cfg.Upload.Ready() {
		uploader = upload.New(cfg.Upload.ClientConfig())
	} else if cfg.Upload.Enabled {
		slog.Warn("upload enabled but OPENMIND_API_URL / OPENMIND_API_KEY not both set; recording locally only")
	}

	controlAddr := config.ControlAddr()
	ctl, currentDirFn, controlCancel := setupControl(controlAddr, recordingsDir, maxBytes, func() string {
		currentMu.Lock()
		defer currentMu.Unlock()
		return current.RealDir()
	})

	// Empty by default: a no-op, so this run behaves exactly as before this feature existed.
	scheduleFile := config.ScheduleFile()
	scheduleCancel := setupSchedule(context.Background(), scheduleFile, ctl)

	// Uploading (never cap enforcement) is gated on ctl.Uploading -- see runRetentionSweeps.
	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	go runRetentionSweeps(sweepCtx, uploader, recordingsDir, bootTimebasePath, currentDirFn, cfg.Retention, ctl)

	rs := startRecorders(cfg, mon, videoHeartbeatNames)

	videoURLs := make([]string, 0, len(cfg.Video))
	for _, vc := range cfg.Video {
		videoURLs = append(videoURLs, vc.Name+"="+vc.RTSPURL)
	}

	slog.Info("recording started",
		"session", sess.RealDir(),
		"session-pending", sess.Pending(),
		"clock-sync", clk.StartSync().String(),
		"robot-type", cfg.RobotType,
		"video-cameras", videoURLs,
		"audio-url", cfg.Audio.RTSPURL,
		"features-enabled", cfg.Features.Enabled(),
		"features-source", cfg.Features.SourcePath,
		"lidar-enabled", cfg.EnableLidar,
		"depth-enabled", cfg.EnableDepth,
		"odom-enabled", cfg.EnableOdom,
		"lowstate-enabled", cfg.EnableLowstate,
		"lidar-topic", cfg.Lidar.DDSTopic,
		"pointcloud-enabled", cfg.EnablePointCloud,
		"pointcloud-topic", cfg.PointCloud.DDSTopic,
		"depth-topic", cfg.Depth.DDSTopic,
		"odom-topic", cfg.Odom.DDSTopic,
		"lowstate-topic", cfg.Lowstate.DDSTopic,
		"net-ping-host", cfg.Network.PingHost,
		"heartbeat", "30s interval; logs only when broken/recovered",
		"session-rotate-interval", cfg.SessionRotateInterval,
		"upload-ready", uploader != nil,
		"control-addr", controlAddr,
		"schedule-file", scheduleFile,
	)
	slog.Info("press Ctrl-C to stop")

	ctl.SetRecording(true)

	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, syscall.SIGINT, syscall.SIGTERM)

	var rotateC <-chan time.Time
	if cfg.SessionRotateInterval > 0 {
		ticker := time.NewTicker(cfg.SessionRotateInterval)
		defer ticker.Stop()
		rotateC = ticker.C
	}

	var uploadWG sync.WaitGroup
	uploadDelete := cfg.Upload.DeleteAfterUpload

loop:
	for {
		select {
		case <-shutdownSignal:
			break loop

		case cmd := <-ctl.RecordingCmds:
			if cmd.Start == ctl.Recording() {
				cmd.Result <- nil // already in the requested state
				continue
			}

			if !cmd.Start {
				// Same as a rotation's close-half, but no next session is opened.
				rs.Stop()
				unregisterHeartbeats(mon, cfg, videoHeartbeatNames)
				finished := sess
				if err := finished.Close(); err != nil {
					slog.Warn("could not finalize session metadata", "dir", finished.RealDir(), "err", err)
				}
				snapshotTimebase(bootTimebasePath, finished.RealDir())
				ctl.SetRecording(false)
				slog.Info("recording paused via control API", "session", finished.RealDir())
				uploadFinishedSessionAsync(&uploadWG, uploader, ctl, recordingsDir, bootTimebasePath, finished, uploadDelete)
				cmd.Result <- nil
				continue
			}

			// Resume: same as a rotation's open-half.
			next, err := session.OpenNext(recordingsDir, clk, watcher.SyncState())
			if err != nil {
				cmd.Result <- err
				continue
			}
			nextCfg := config.Load(next.Dir())
			next.SetRobotType(string(nextCfg.RobotType))

			currentMu.Lock()
			current = next
			currentMu.Unlock()

			sess, cfg = next, nextCfg
			registerHeartbeats(mon, cfg, videoHeartbeatNames)
			rs = startRecorders(cfg, mon, videoHeartbeatNames)
			ctl.SetRecording(true)
			slog.Info("recording resumed via control API", "session", sess.RealDir())
			cmd.Result <- nil

		case <-rotateC:
			if !ctl.Recording() {
				continue // paused via the control API; nothing to rotate
			}

			rs.Stop()
			finished := sess
			if err := finished.Close(); err != nil {
				slog.Warn("could not finalize session metadata", "dir", finished.RealDir(), "err", err)
			}
			snapshotTimebase(bootTimebasePath, finished.RealDir())

			next, err := session.OpenNext(recordingsDir, clk, watcher.SyncState())
			if err != nil {
				slog.Error("cannot open next session; stopping", "err", err)
				break loop
			}
			nextCfg := config.Load(next.Dir())
			next.SetRobotType(string(nextCfg.RobotType))

			currentMu.Lock()
			current = next
			currentMu.Unlock()

			sess, cfg = next, nextCfg
			registerHeartbeats(mon, cfg, videoHeartbeatNames) // resets the grace period for the fresh streams below
			rs = startRecorders(cfg, mon, videoHeartbeatNames)

			slog.Info("session rotated", "session", sess.RealDir())

			uploadFinishedSessionAsync(&uploadWG, uploader, ctl, recordingsDir, bootTimebasePath, finished, uploadDelete)
		}
	}

	slog.Info("shutting down…")

	hbCancel()
	sweepCancel()
	scheduleCancel()
	controlCancel()
	if ctl.Recording() {
		rs.Stop()
		if err := sess.Close(); err != nil {
			slog.Warn("could not finalize session metadata", "dir", sess.RealDir(), "err", err)
		}
		snapshotTimebase(bootTimebasePath, sess.RealDir())

		if uploader != nil && ctl.Uploading() {
			opts := uploadOptions(bootTimebasePath, sess.RealDir())
			uploadSession(uploader, sess.RealDir(), apiSessionDir(recordingsDir, sess.RealDir()), time.Unix(0, sess.StartUnixNs()), uploadDelete, opts)
		}
	}
	uploadWG.Wait()

	clkCancel()

	if sess.Pending() {
		slog.Warn("session is still undated: the clock never synchronized while recording",
			"dir", sess.RealDir(),
			"note", "it will be dated on a later start if it ever syncs")
	}
}

// setupControl builds the control-plane state and starts its HTTP server on addr.
func setupControl(addr, recordingsDir string, maxBytes int64, rawCurrentDir func() string) (ctl *control.State, currentDirFn func() string, cancel context.CancelFunc) {
	ctl = control.New()

	currentDirFn = func() string {
		if !ctl.Recording() {
			return ""
		}
		return rawCurrentDir()
	}

	ctl.Extra = func() control.Extra {
		bytes, _ := dirSize(recordingsDir)
		return control.Extra{
			CurrentSession:  currentDirFn(),
			RecordingsBytes: bytes,
			MaxBytes:        maxBytes,
			PendingUploads:  pendingUploadCount(recordingsDir),
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := control.Serve(ctx, addr, ctl); err != nil {
			slog.Error("control server stopped", "addr", addr, "err", err)
		}
	}()

	return ctl, currentDirFn, cancel
}

// uploadFinishedSessionAsync kicks off finished's upload in the background if uploading is enabled.
func uploadFinishedSessionAsync(wg *sync.WaitGroup, uploader *upload.Client, ctl *control.State, recordingsDir, bootTimebasePath string, finished *session.Session, uploadDelete bool) {
	if uploader == nil || !ctl.Uploading() {
		return
	}
	opts := uploadOptions(bootTimebasePath, finished.RealDir())
	wg.Add(1)
	go func(dir, apiDir string, startedAt time.Time, opts upload.Options) {
		defer wg.Done()
		uploadSession(uploader, dir, apiDir, startedAt, uploadDelete, opts)
	}(finished.RealDir(), apiSessionDir(recordingsDir, finished.RealDir()), time.Unix(0, finished.StartUnixNs()), opts)
}

// uploadSession uploads one finished session directory; on failure the files are kept locally for a later retry.
func uploadSession(client *upload.Client, dir, sessionDir string, startedAt time.Time, deleteAfter bool, opts upload.Options) {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()

	if err := client.UploadSession(ctx, dir, sessionDir, startedAt, opts); err != nil {
		slog.Error("session upload failed; files kept locally for retry", "dir", dir, "err", err)
		return
	}
	slog.Info("session uploaded", "dir", dir, "session_dir", sessionDir)
	markUploaded(dir)

	if deleteAfter && opts.PreserveJSONL == "" {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("could not delete uploaded session directory", "dir", dir, "err", err)
		}
	}
}

// uploadOptions protects the live clock journal when dir is the boot session's own directory.
func uploadOptions(bootTimebasePath, dir string) upload.Options {
	if bootSessionDir(bootTimebasePath, dir) {
		return upload.Options{PreserveJSONL: clock.TimebaseName}
	}
	return upload.Options{}
}

// bootSessionDir reports whether dir holds the live boot-relative clock journal.
func bootSessionDir(bootTimebasePath, dir string) bool {
	want, err := os.Stat(bootTimebasePath)
	if err != nil {
		return false
	}
	got, err := os.Stat(filepath.Join(dir, clock.TimebaseName))
	if err != nil {
		return false
	}
	return os.SameFile(want, got)
}

// apiSessionDir builds the openmind-api's session_dir grouping key from a session's real directory.
func apiSessionDir(recordingsDir, realDir string) string {
	rel, err := filepath.Rel(recordingsDir, realDir)
	if err != nil {
		rel = filepath.Base(realDir)
	}
	return filepath.ToSlash(filepath.Join(filepath.Base(recordingsDir), rel))
}

// snapshotTimebase copies the process-wide clock journal into a rotated-away session's own directory.
func snapshotTimebase(bootPath, sessionDir string) {
	if bootSessionDir(bootPath, sessionDir) {
		return
	}
	raw, err := os.ReadFile(bootPath)
	if err != nil {
		slog.Warn("could not snapshot clock timebase for rotated session", "err", err)
		return
	}
	dst := filepath.Join(sessionDir, clock.TimebaseName)
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		slog.Warn("could not write clock timebase snapshot", "dst", dst, "err", err)
	}
}

func registerHeartbeats(mon *heartbeat.Monitor, cfg config.Config, videoHeartbeatNames []string) {
	if cfg.EnableLidar {
		mon.Register(lidar.HeartbeatName, 10) // ~10 Hz nominal
	}
	if cfg.EnablePointCloud {
		mon.Register(pointcloud.HeartbeatName, 10) // ~10 Hz nominal
	}
	if cfg.EnableDepth {
		mon.Register(depth.HeartbeatName, 10) // measured 14.9 Hz on a G1; floor at 10
	}
	if cfg.EnableOdom {
		mon.Register(odom.HeartbeatName, 30) // Go2 ~150, G1 ~495; 30 = safe floor
	}
	if cfg.EnableLowstate {
		mon.Register(lowstate.HeartbeatName, 400) // Go2 ~500, G1 ~1053; 400 = safe floor
	}
	mon.Register(network.HeartbeatName, 0) // 0 = liveness check only
	mon.Register(audio.HeartbeatName, 0)   // 0 = liveness check only
	if cfg.Features.Enabled() {
		mon.Register(features.HeartbeatName, 0) // 0 = liveness check only
	}
	for _, name := range videoHeartbeatNames {
		mon.Register(name, 0)
	}
}

// unregisterHeartbeats undoes registerHeartbeats for cfg's streams.
func unregisterHeartbeats(mon *heartbeat.Monitor, cfg config.Config, videoHeartbeatNames []string) {
	if cfg.EnableLidar {
		mon.Unregister(lidar.HeartbeatName)
	}
	if cfg.EnablePointCloud {
		mon.Unregister(pointcloud.HeartbeatName)
	}
	if cfg.EnableDepth {
		mon.Unregister(depth.HeartbeatName)
	}
	if cfg.EnableOdom {
		mon.Unregister(odom.HeartbeatName)
	}
	if cfg.EnableLowstate {
		mon.Unregister(lowstate.HeartbeatName)
	}
	mon.Unregister(network.HeartbeatName)
	mon.Unregister(audio.HeartbeatName)
	if cfg.Features.Enabled() {
		mon.Unregister(features.HeartbeatName)
	}
	for _, name := range videoHeartbeatNames {
		mon.Unregister(name)
	}
}

// recorderSet is every stream started for one session; disabled streams are left nil.
type recorderSet struct {
	videoStreams     []*video.VideoRTSPStream
	audioStream      *audio.AudioRTSPStream
	featureStream    *features.FeatureStream
	lidarStream      *lidar.LidarStream
	pointCloudStream *pointcloud.PointCloudStream
	depthStream      *depth.DepthStream
	odomStream       *odom.OdomStream
	lowstateStream   *lowstate.LowstateStream
	networkStream    *network.NetworkStream
}

// startRecorders builds and starts every stream for cfg's session directory.
func startRecorders(cfg config.Config, mon *heartbeat.Monitor, videoHeartbeatNames []string) *recorderSet {
	rs := &recorderSet{}

	rs.videoStreams = make([]*video.VideoRTSPStream, 0, len(cfg.Video))
	for i, vc := range cfg.Video {
		vcfg := vc.VideoStreamConfig()
		vcfg.Monitor = mon
		vcfg.HeartbeatName = videoHeartbeatNames[i]
		if vcfg.FramesFile == "" {
			ext := filepath.Ext(vcfg.OutputFile)
			stem := vcfg.OutputFile[:len(vcfg.OutputFile)-len(ext)]
			vcfg.FramesFile = stem + "_frames.csv"
		}
		rs.videoStreams = append(rs.videoStreams, video.New(vcfg))
	}

	audioCfg := cfg.Audio.AudioStreamConfig()
	audioCfg.Monitor = mon
	if audioCfg.FramesFile == "" {
		ext := filepath.Ext(audioCfg.OutputFile)
		stem := audioCfg.OutputFile[:len(audioCfg.OutputFile)-len(ext)]
		audioCfg.FramesFile = stem + "_packets.csv"
	}
	rs.audioStream = audio.New(audioCfg)

	if cfg.Features.Enabled() {
		featureCfg := cfg.Features.FeatureStreamConfig()
		featureCfg.Monitor = mon
		rs.featureStream = features.New(featureCfg)
	}

	if cfg.EnableLidar {
		lidarCfg := cfg.Lidar.LidarStreamConfig()
		lidarCfg.Monitor = mon
		rs.lidarStream = lidar.New(lidarCfg)
	}

	if cfg.EnablePointCloud {
		pointCloudCfg := cfg.PointCloud.PointCloudStreamConfig()
		pointCloudCfg.Monitor = mon
		rs.pointCloudStream = pointcloud.New(pointCloudCfg)
	}

	if cfg.EnableDepth {
		depthCfg := cfg.Depth.DepthStreamConfig()
		depthCfg.Monitor = mon
		rs.depthStream = depth.New(depthCfg)
	}

	if cfg.EnableOdom {
		odomCfg := cfg.Odom.OdomStreamConfig()
		odomCfg.Monitor = mon
		rs.odomStream = odom.New(odomCfg)
	}

	if cfg.EnableLowstate {
		lowstateCfg := cfg.Lowstate.LowstateStreamConfig()
		lowstateCfg.Monitor = mon
		rs.lowstateStream = lowstate.New(lowstateCfg)
	}

	networkCfg := cfg.Network.NetworkStreamConfig()
	networkCfg.Monitor = mon
	rs.networkStream = network.New(networkCfg)

	for _, vs := range rs.videoStreams {
		vs.Start()
	}
	rs.audioStream.Start()
	if rs.featureStream != nil {
		rs.featureStream.Start()
	}
	if rs.lidarStream != nil {
		rs.lidarStream.Start()
	}
	if rs.pointCloudStream != nil {
		rs.pointCloudStream.Start()
	}
	if rs.depthStream != nil {
		rs.depthStream.Start()
	}
	if rs.odomStream != nil {
		rs.odomStream.Start()
	}
	if rs.lowstateStream != nil {
		rs.lowstateStream.Start()
	}
	rs.networkStream.Start()

	return rs
}

// Stop stops every stream in the set, blocking until each has flushed to disk.
func (rs *recorderSet) Stop() {
	for _, vs := range rs.videoStreams {
		vs.Stop()
	}
	rs.audioStream.Stop()
	if rs.featureStream != nil {
		rs.featureStream.Stop()
	}
	if rs.lidarStream != nil {
		rs.lidarStream.Stop()
	}
	if rs.pointCloudStream != nil {
		rs.pointCloudStream.Stop()
	}
	if rs.depthStream != nil {
		rs.depthStream.Stop()
	}
	if rs.odomStream != nil {
		rs.odomStream.Stop()
	}
	if rs.lowstateStream != nil {
		rs.lowstateStream.Stop()
	}
	rs.networkStream.Stop()
}
