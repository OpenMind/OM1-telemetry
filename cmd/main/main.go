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
	"om1-telemetry/internal/retention"
	"om1-telemetry/internal/schedule"
	"om1-telemetry/internal/session"
	"om1-telemetry/internal/traces"
	"om1-telemetry/internal/upload"
	"om1-telemetry/internal/video"
)

// scheduleTickInterval is how often the schedule reconciler checks for a state change.
const scheduleTickInterval = 30 * time.Second

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

	if !cfg.Collect {
		slog.Info("data collection disabled via ENABLE_COLLECTION=false, exiting")
		return
	}

	videoHeartbeatNames := make([]string, len(cfg.Video))
	for i, vc := range cfg.Video {
		videoHeartbeatNames[i] = "video_" + vc.Name
	}

	mon := heartbeat.NewMonitor(30 * time.Second)

	var rs *recorderSet
	streamsFn := func() *persistentStreams {
		if rs == nil {
			return nil
		}
		return &rs.persistent
	}

	registerHeartbeats(mon, cfg, videoHeartbeatNames, streamsFn)

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

	ctl := control.New()
	currentDirFn := func() string {
		if !ctl.Recording() {
			return ""
		}
		currentMu.Lock()
		defer currentMu.Unlock()
		return current.RealDir()
	}

	// Empty by default: Load returns (nil, nil), so this run behaves exactly
	// as before this feature existed.
	scheduleFile := config.ScheduleFile()
	scheduleCfg, err := schedule.Load(scheduleFile)
	if err != nil {
		slog.Error("could not load schedule; recording and uploading stay on continuously",
			"path", scheduleFile, "err", err)
		scheduleCfg = nil
	} else if scheduleCfg != nil {
		slog.Info("schedule loaded", "path", scheduleFile)
	}

	// Uploading (never cap enforcement) is gated on ctl.Uploading -- see retention.RunSweeps.
	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	go retention.RunSweeps(sweepCtx, uploader, recordingsDir, bootTimebasePath, currentDirFn, cfg.Retention, ctl)

	rs = startRecorders(cfg, mon, recordingsDir, sess, videoHeartbeatNames)

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

	// A nil scheduleC blocks forever in the select below, so an unset/unloadable
	// schedule leaves recording and uploading on continuously -- the same as if
	// this feature didn't exist.
	var scheduleC <-chan time.Time
	if scheduleCfg != nil {
		ticker := time.NewTicker(scheduleTickInterval)
		defer ticker.Stop()
		scheduleC = ticker.C
	}

	var uploadWG sync.WaitGroup
	uploadDelete := cfg.Upload.DeleteAfterUpload

	// pauseRecording is the schedule's close-half: same as a rotation's
	// close-half, but no next session is opened.
	pauseRecording := func() {
		rs.Stop()
		unregisterHeartbeats(mon, cfg, videoHeartbeatNames)
		finished := sess
		if err := finished.Close(); err != nil {
			slog.Warn("could not finalize session metadata", "dir", finished.RealDir(), "err", err)
		}
		retention.SnapshotTimebase(bootTimebasePath, finished.RealDir())
		ctl.SetRecording(false)
		slog.Info("recording paused by schedule", "session", finished.RealDir())
		persistent := rs.persistent
		finishedStart := time.Unix(0, finished.StartUnixNs())
		retention.UploadFinishedSessionAsync(&uploadWG, uploader, ctl, recordingsDir, bootTimebasePath, finished, uploadDelete,
			func(ctx context.Context) { persistent.awaitSegments(ctx, finishedStart) })
	}

	// resumeRecording is the schedule's open-half: same as a rotation's open-half.
	resumeRecording := func() error {
		next, err := session.OpenNext(recordingsDir, clk, watcher.SyncState())
		if err != nil {
			return err
		}
		nextCfg := config.Load(next.Dir())
		next.SetRobotType(string(nextCfg.RobotType))

		currentMu.Lock()
		current = next
		currentMu.Unlock()

		sess, cfg = next, nextCfg
		registerHeartbeats(mon, cfg, videoHeartbeatNames, streamsFn)
		rs = startRecorders(cfg, mon, recordingsDir, sess, videoHeartbeatNames)
		ctl.SetRecording(true)
		slog.Info("recording resumed by schedule", "session", sess.RealDir())
		return nil
	}

	// reconcileSchedule brings recording/uploading in line with scheduleCfg;
	// a no-op on any axis already in the wanted state.
	reconcileSchedule := func() {
		now := time.Now()

		if want := scheduleCfg.Recording.Active(now); want != ctl.Recording() {
			if want {
				if err := resumeRecording(); err != nil {
					slog.Error("schedule: could not resume recording", "err", err)
				}
			} else {
				pauseRecording()
			}
		}

		if want := scheduleCfg.Upload.Active(now); want != ctl.Uploading() {
			ctl.SetUploading(want)
			if want {
				ctl.TriggerUpload()
			}
		}
	}

	if scheduleCfg != nil {
		reconcileSchedule()
	}

loop:
	for {
		select {
		case <-shutdownSignal:
			break loop

		case <-scheduleC:
			reconcileSchedule()

		case <-rotateC:
			if !ctl.Recording() {
				continue
			}

			rs.stopEphemeral()
			finished := sess
			if err := finished.Close(); err != nil {
				slog.Warn("could not finalize session metadata", "dir", finished.RealDir(), "err", err)
			}
			retention.SnapshotTimebase(bootTimebasePath, finished.RealDir())

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
			registerHeartbeats(mon, cfg, videoHeartbeatNames, streamsFn)

			persistent := rs.persistent
			rs = startEphemeral(cfg, mon)
			rs.persistent = persistent
			rs.persistent.rotate(cfg, sess)

			slog.Info("session rotated", "session", sess.RealDir())

			finishedStart := time.Unix(0, finished.StartUnixNs())
			retention.UploadFinishedSessionAsync(&uploadWG, uploader, ctl, recordingsDir, bootTimebasePath, finished, uploadDelete,
				func(ctx context.Context) { persistent.awaitSegments(ctx, finishedStart) })
		}
	}

	slog.Info("shutting down…")

	hbCancel()
	sweepCancel()
	if ctl.Recording() {
		rs.Stop()
		if err := sess.Close(); err != nil {
			slog.Warn("could not finalize session metadata", "dir", sess.RealDir(), "err", err)
		}
		retention.SnapshotTimebase(bootTimebasePath, sess.RealDir())

		if uploader != nil && ctl.Uploading() {
			opts := retention.UploadOptions(bootTimebasePath, sess.RealDir())
			// No awaitReady needed: rs.Stop() above already drains ffmpeg's
			// final segment (and its relocation) before returning.
			retention.UploadSession(ctl, uploader, sess.RealDir(), retention.APISessionDir(recordingsDir, sess.RealDir()), time.Unix(0, sess.StartUnixNs()), uploadDelete, opts, nil)
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

// registerHeartbeats registers every stream's heartbeat check, wiring the
// five DDS-backed streams up with a reconnect callback.
func registerHeartbeats(mon *heartbeat.Monitor, cfg config.Config, videoHeartbeatNames []string, streams func() *persistentStreams) {
	if cfg.EnableLidar {
		mon.RegisterRecoverable(lidar.HeartbeatName, 10, func() {
			if p := streams(); p != nil && p.lidarStream != nil {
				p.lidarStream.Reconnect()
			}
		})
	}
	if cfg.EnablePointCloud {
		mon.RegisterRecoverable(pointcloud.HeartbeatName, 10, func() {
			if p := streams(); p != nil && p.pointCloudStream != nil {
				p.pointCloudStream.Reconnect()
			}
		})
	}
	if cfg.EnableDepth {
		mon.RegisterRecoverable(depth.HeartbeatName, 10, func() {
			if p := streams(); p != nil && p.depthStream != nil {
				p.depthStream.Reconnect()
			}
		})
	}
	if cfg.EnableOdom {
		mon.RegisterRecoverable(odom.HeartbeatName, 30, func() {
			if p := streams(); p != nil && p.odomStream != nil {
				p.odomStream.Reconnect()
			}
		})
	}
	if cfg.EnableLowstate {
		mon.RegisterRecoverable(lowstate.HeartbeatName, 400, func() {
			if p := streams(); p != nil && p.lowstateStream != nil {
				p.lowstateStream.Reconnect()
			}
		})
	}
	mon.Register(network.HeartbeatName, 0) // 0 = liveness check only
	mon.Register(audio.HeartbeatName, 0)   // 0 = liveness check only
	if cfg.Features.Enabled() {
		mon.Register(features.HeartbeatName, 0) // 0 = liveness check only
	}
	if cfg.Traces.Enabled {
		mon.Register(traces.HeartbeatName, 0)
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
	if cfg.Traces.Enabled {
		mon.Unregister(traces.HeartbeatName)
	}
	for _, name := range videoHeartbeatNames {
		mon.Unregister(name)
	}
}

// persistentStreams is every recorder that stays alive across a session
// rotation (see rotate): the DDS streams and the video/audio RTSP streams.
// Their subscriptions and connections are expensive to reestablish, so
// rotation only swaps their output files instead of stopping and
// restarting them; only a real pause/resume tears them down and rebuilds
// them.
type persistentStreams struct {
	lidarStream      *lidar.LidarStream
	pointCloudStream *pointcloud.PointCloudStream
	depthStream      *depth.DepthStream
	odomStream       *odom.OdomStream
	lowstateStream   *lowstate.LowstateStream
	videoStreams     []*video.VideoRTSPStream
	audioStream      *audio.AudioRTSPStream
	traceStream      *traces.TraceStream
}

func newPersistentStreams(cfg config.Config, mon *heartbeat.Monitor, recordingsDir string, sess *session.Session, videoHeartbeatNames []string) persistentStreams {
	var p persistentStreams
	sessionStart := time.Unix(0, sess.StartUnixNs())
	sessionRealDir := sess.RealDir()

	if cfg.EnableLidar {
		lidarCfg := cfg.Lidar.LidarStreamConfig()
		lidarCfg.Monitor = mon
		p.lidarStream = lidar.New(lidarCfg)
	}
	if cfg.EnablePointCloud {
		pointCloudCfg := cfg.PointCloud.PointCloudStreamConfig()
		pointCloudCfg.Monitor = mon
		p.pointCloudStream = pointcloud.New(pointCloudCfg)
	}
	if cfg.EnableDepth {
		depthCfg := cfg.Depth.DepthStreamConfig()
		depthCfg.Monitor = mon
		p.depthStream = depth.New(depthCfg)
	}
	if cfg.EnableOdom {
		odomCfg := cfg.Odom.OdomStreamConfig()
		odomCfg.Monitor = mon
		p.odomStream = odom.New(odomCfg)
	}
	if cfg.EnableLowstate {
		lowstateCfg := cfg.Lowstate.LowstateStreamConfig()
		lowstateCfg.Monitor = mon
		p.lowstateStream = lowstate.New(lowstateCfg)
	}
	if cfg.Traces.Enabled {
		tracesCfg := cfg.Traces.TraceStreamConfig()
		tracesCfg.Monitor = mon
		p.traceStream = traces.New(tracesCfg)
	}

	p.videoStreams = make([]*video.VideoRTSPStream, 0, len(cfg.Video))
	for i, vc := range cfg.Video {
		vcfg := vc.VideoStreamConfig()
		vcfg.Monitor = mon
		vcfg.HeartbeatName = videoHeartbeatNames[i]
		vcfg.RotateInterval = cfg.SessionRotateInterval
		vcfg.ScratchDir = filepath.Join(recordingsDir, ".rotating", vc.Name)
		vcfg.SessionStart = sessionStart
		vcfg.OutputFile = realPath(vcfg.OutputFile, sessionRealDir)
		vcfg.TimestampsFile = realPath(vcfg.TimestampsFile, sessionRealDir)
		if vcfg.FramesFile == "" {
			vcfg.FramesFile = framesFileFor(vcfg.OutputFile, "_frames.csv")
		} else {
			vcfg.FramesFile = realPath(vcfg.FramesFile, sessionRealDir)
		}
		p.videoStreams = append(p.videoStreams, video.New(vcfg))
	}

	audioCfg := cfg.Audio.AudioStreamConfig()
	audioCfg.Monitor = mon
	audioCfg.RotateInterval = cfg.SessionRotateInterval
	audioCfg.ScratchDir = filepath.Join(recordingsDir, ".rotating", "audio")
	audioCfg.SessionStart = sessionStart
	audioCfg.OutputFile = realPath(audioCfg.OutputFile, sessionRealDir)
	audioCfg.TimestampsFile = realPath(audioCfg.TimestampsFile, sessionRealDir)
	if audioCfg.FramesFile == "" {
		audioCfg.FramesFile = framesFileFor(audioCfg.OutputFile, "_packets.csv")
	} else {
		audioCfg.FramesFile = realPath(audioCfg.FramesFile, sessionRealDir)
	}
	p.audioStream = audio.New(audioCfg)

	return p
}

func (p *persistentStreams) start() {
	if p.lidarStream != nil {
		p.lidarStream.Start()
	}
	if p.pointCloudStream != nil {
		p.pointCloudStream.Start()
	}
	if p.depthStream != nil {
		p.depthStream.Start()
	}
	if p.odomStream != nil {
		p.odomStream.Start()
	}
	if p.lowstateStream != nil {
		p.lowstateStream.Start()
	}
	if p.traceStream != nil {
		p.traceStream.Start()
	}
	for _, vs := range p.videoStreams {
		vs.Start()
	}
	p.audioStream.Start()
}

func (p *persistentStreams) stop() {
	if p.lidarStream != nil {
		p.lidarStream.Stop()
	}
	if p.pointCloudStream != nil {
		p.pointCloudStream.Stop()
	}
	if p.depthStream != nil {
		p.depthStream.Stop()
	}
	if p.odomStream != nil {
		p.odomStream.Stop()
	}
	if p.lowstateStream != nil {
		p.lowstateStream.Stop()
	}
	if p.traceStream != nil {
		p.traceStream.Stop()
	}
	for _, vs := range p.videoStreams {
		vs.Stop()
	}
	p.audioStream.Stop()
}

// rotate switches every persistent stream's output to sess's session
// directory without resubscribing or reconnecting.
func (p *persistentStreams) rotate(cfg config.Config, sess *session.Session) {
	sessionStart := time.Unix(0, sess.StartUnixNs())
	sessionRealDir := sess.RealDir()
	if p.lidarStream != nil {
		if err := p.lidarStream.Rotate(cfg.Lidar.DataFile, cfg.Lidar.TimestampsFile); err != nil {
			slog.Error("lidar: rotate failed", "err", err)
		}
	}
	if p.pointCloudStream != nil {
		if err := p.pointCloudStream.Rotate(cfg.PointCloud.DataFile, cfg.PointCloud.TimestampsFile); err != nil {
			slog.Error("pointcloud: rotate failed", "err", err)
		}
	}
	if p.depthStream != nil {
		if err := p.depthStream.Rotate(cfg.Depth.DataFile, cfg.Depth.TimestampsFile); err != nil {
			slog.Error("depth: rotate failed", "err", err)
		}
	}
	if p.odomStream != nil {
		if err := p.odomStream.Rotate(cfg.Odom.DataFile, cfg.Odom.TimestampsFile); err != nil {
			slog.Error("odom: rotate failed", "err", err)
		}
	}
	if p.lowstateStream != nil {
		if err := p.lowstateStream.Rotate(cfg.Lowstate.DataFile, cfg.Lowstate.TimestampsFile); err != nil {
			slog.Error("lowstate: rotate failed", "err", err)
		}
	}
	if p.traceStream != nil {
		if err := p.traceStream.Rotate(cfg.Traces.OutputFile); err != nil {
			slog.Error("traces: rotate failed", "err", err)
		}
	}
	for i, vs := range p.videoStreams {
		vc := cfg.Video[i]
		out := realPath(vc.OutputFile, sessionRealDir)
		ts := realPath(vc.TimestampsFile, sessionRealDir)
		vs.Rotate(sessionStart, ts, framesFileFor(out, "_frames.csv"))
	}
	audioOut := realPath(cfg.Audio.OutputFile, sessionRealDir)
	audioTs := realPath(cfg.Audio.TimestampsFile, sessionRealDir)
	p.audioStream.Rotate(sessionStart, audioTs, framesFileFor(audioOut, "_packets.csv"))
}

// awaitSegments blocks until every video stream and the audio stream have
// relocated the segment belonging to the session that started at
// sessionStart, or ctx ends. Meant to run right before that session gets
// uploaded: video/audio only relocate a segment once ffmpeg reports it
// closed, which happens a full segment_time (== the rotation interval)
// after the segment began -- i.e. at roughly the same moment as the next
// rotation, not this one. Uploading on rotation without this wait races
// that and silently ships the session without its video/audio.
func (p *persistentStreams) awaitSegments(ctx context.Context, sessionStart time.Time) {
	var wg sync.WaitGroup
	for _, vs := range p.videoStreams {
		wg.Add(1)
		go func(vs *video.VideoRTSPStream) {
			defer wg.Done()
			vs.WaitSegment(ctx, sessionStart)
		}(vs)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.audioStream.WaitSegment(ctx, sessionStart)
	}()
	wg.Wait()
}

// realPath rewrites path to sit in realDir instead of whatever directory it
// was built from.
func realPath(path, realDir string) string {
	return filepath.Join(realDir, filepath.Base(path))
}

// framesFileFor derives the per-frame timestamps CSV path that sits
// alongside outputFile; config.go's VideoConfig/AudioConfig don't carry a
// FramesFile of their own.
func framesFileFor(outputFile, suffix string) string {
	ext := filepath.Ext(outputFile)
	return outputFile[:len(outputFile)-len(ext)] + suffix
}

// recorderSet is every stream started for one session; disabled streams are left nil.
type recorderSet struct {
	featureStream *features.FeatureStream
	networkStream *network.NetworkStream
	persistent    persistentStreams
}

// startEphemeral builds and starts every stream that gets rebuilt on each
// session rotation -- everything except persistentStreams.
func startEphemeral(cfg config.Config, mon *heartbeat.Monitor) *recorderSet {
	rs := &recorderSet{}

	if cfg.Features.Enabled() {
		featureCfg := cfg.Features.FeatureStreamConfig()
		featureCfg.Monitor = mon
		rs.featureStream = features.New(featureCfg)
	}

	networkCfg := cfg.Network.NetworkStreamConfig()
	networkCfg.Monitor = mon
	rs.networkStream = network.New(networkCfg)

	if rs.featureStream != nil {
		rs.featureStream.Start()
	}
	rs.networkStream.Start()

	return rs
}

// stopEphemeral stops every stream that gets rebuilt on each session
// rotation, blocking until each has flushed to disk. rs.persistent is left
// running; see persistentStreams.rotate.
func (rs *recorderSet) stopEphemeral() {
	if rs.featureStream != nil {
		rs.featureStream.Stop()
	}
	rs.networkStream.Stop()
}

// startRecorders builds and starts every stream for cfg's session directory,
// including the persistent streams.
func startRecorders(cfg config.Config, mon *heartbeat.Monitor, recordingsDir string, sess *session.Session, videoHeartbeatNames []string) *recorderSet {
	rs := startEphemeral(cfg, mon)
	rs.persistent = newPersistentStreams(cfg, mon, recordingsDir, sess, videoHeartbeatNames)
	rs.persistent.start()
	return rs
}

// Stop stops every stream in the set, blocking until each has flushed to disk.
func (rs *recorderSet) Stop() {
	rs.stopEphemeral()
	rs.persistent.stop()
}
