// Package control implements a local control plane for starting/stopping
// recording and pausing/resuming uploading without restarting the process.
package control

import (
	"context"
	"sync/atomic"
)

// RecordingCmd asks main's event loop to start or stop recording.
type RecordingCmd struct {
	Start  bool
	Result chan error
}

// Extra supplies dynamic status detail (current session, disk usage) via a
// callback main sets, so this package doesn't need to know about sessions.
type Extra struct {
	CurrentSession  string
	RecordingsBytes int64
	MaxBytes        int64
	PendingUploads  int
}

// State is the shared control-plane state: recording and uploading, both
// on by default.
type State struct {
	recording atomic.Bool
	uploading atomic.Bool

	RecordingCmds chan RecordingCmd
	UploadTrigger chan struct{}

	Extra func() Extra
}

// New returns a State with both recording and uploading on.
func New() *State {
	s := &State{
		RecordingCmds: make(chan RecordingCmd),
		UploadTrigger: make(chan struct{}, 1),
	}
	s.recording.Store(true)
	s.uploading.Store(true)
	return s
}

// Recording reports whether recording is currently active.
func (s *State) Recording() bool { return s.recording.Load() }

// SetRecording records that main's event loop has started or stopped recording.
func (s *State) SetRecording(v bool) { s.recording.Store(v) }

// Uploading reports whether uploads are currently enabled.
func (s *State) Uploading() bool { return s.uploading.Load() }

// SetUploading enables or disables uploading.
func (s *State) SetUploading(v bool) { s.uploading.Store(v) }

// TriggerUpload asks for an immediate catch-up sweep; a no-op if one is already queued.
func (s *State) TriggerUpload() {
	select {
	case s.UploadTrigger <- struct{}{}:
	default:
	}
}

// SendRecordingCmd sends a start/stop request and blocks until it's handled or ctx is done.
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
