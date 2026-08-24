// Package control implements a small local control plane that lets an
// operator start/stop recording and pause/resume uploading on a running
// recorder without restarting the process. See cmd/main for how its
// channels drive the main event loop and the retention sweep.
package control

import (
	"context"
	"sync/atomic"
)

// RecordingCmd asks main's event loop to start or stop recording. Result
// receives nil once the transition (or a no-op, if already in the
// requested state) has completed, so a caller can report back
// synchronously instead of just queuing the request and hoping.
type RecordingCmd struct {
	Start  bool
	Result chan error
}

// Extra supplies the dynamic status detail State can't compute on its own
// -- current session, disk usage -- via a callback main sets once that
// state is available, so this package does not need to know about
// sessions or retention.
type Extra struct {
	CurrentSession  string
	RecordingsBytes int64
	MaxBytes        int64
	PendingUploads  int
}

// State is the shared control-plane state: two independent switches,
// recording and uploading, both on by default so a freshly started
// container behaves exactly as it always has until an operator asks
// otherwise.
//
// Recording is read-only from the HTTP handlers' side -- it only reflects
// what main's event loop has actually done, set via SetRecording after a
// RecordingCmd completes. Uploading is the actual gate: cmd/main's upload
// call sites and the retention sweep's catch-up ticker check it directly,
// so toggling it takes effect the next time either checks, without
// round-tripping through the event loop.
type State struct {
	recording atomic.Bool
	uploading atomic.Bool

	// RecordingCmds carries start/stop requests into main's event loop.
	RecordingCmds chan RecordingCmd

	// UploadTrigger asks the retention sweep's upload ticker to run a
	// catch-up pass immediately instead of waiting for its next tick.
	// Buffered so a trigger arriving while a sweep is already in flight is
	// not lost, but repeated triggers before it's consumed collapse into
	// one.
	UploadTrigger chan struct{}

	// Extra, if set, supplies Status's dynamic fields.
	Extra func() Extra
}

// New returns a State with both recording and uploading on, matching a
// freshly started container's default behavior.
func New() *State {
	s := &State{
		RecordingCmds: make(chan RecordingCmd),
		UploadTrigger: make(chan struct{}, 1),
	}
	s.recording.Store(true)
	s.uploading.Store(true)
	return s
}

// Recording reports whether main's event loop currently has recording
// active.
func (s *State) Recording() bool { return s.recording.Load() }

// SetRecording is called only by main's event loop, after it has actually
// started or stopped recording, so Recording never reports a state that
// hasn't taken effect yet.
func (s *State) SetRecording(v bool) { s.recording.Store(v) }

// Uploading reports whether uploads are currently enabled.
func (s *State) Uploading() bool { return s.uploading.Load() }

// SetUploading enables or disables uploading. Safe to call directly from
// an HTTP handler: unlike recording, there is no in-flight stream state to
// coordinate, so the change is visible to the retention sweep and to
// cmd/main's upload call sites the next time either checks it.
func (s *State) SetUploading(v bool) { s.uploading.Store(v) }

// TriggerUpload asks for an immediate catch-up sweep. Non-blocking: if one
// is already queued, this is a no-op -- the queued trigger will still run
// a sweep covering everything outstanding, including whatever prompted
// this call.
func (s *State) TriggerUpload() {
	select {
	case s.UploadTrigger <- struct{}{}:
	default:
	}
}

// SendRecordingCmd sends a start/stop request to main's event loop and
// blocks until it's handled or ctx is done. Safe to call concurrently;
// main's event loop processes commands one at a time.
func (s *State) SendRecordingCmd(ctx context.Context, start bool) error {
	cmd := RecordingCmd{Start: start, Result: make(chan error, 1)}
	select {
	case s.RecordingCmds <- cmd:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-cmd.Result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
