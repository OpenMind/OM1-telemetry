// Package control holds the recording/uploading state that main's event
// loop and the retention sweep coordinate through, so a schedule can start
// or stop recording and pause or resume uploading without restarting the
// process.
package control

import "sync/atomic"

// State is the shared control-plane state: recording and uploading, both
// on by default.
type State struct {
	recording atomic.Bool
	uploading atomic.Bool

	// UploadTrigger asks the retention sweep for an immediate catch-up pass.
	UploadTrigger chan struct{}
}

// New returns a State with both recording and uploading on.
func New() *State {
	s := &State{
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
