package control

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNew_defaultsBothOn(t *testing.T) {
	s := New()
	require.True(t, s.Recording())
	require.True(t, s.Uploading())
}

func TestSetRecording_reflectsImmediately(t *testing.T) {
	s := New()
	s.SetRecording(false)
	require.False(t, s.Recording())
	s.SetRecording(true)
	require.True(t, s.Recording())
}

func TestSetUploading_reflectsImmediately(t *testing.T) {
	s := New()
	s.SetUploading(false)
	require.False(t, s.Uploading())
}

func TestTriggerUpload_nonBlockingAndCoalesces(t *testing.T) {
	s := New()
	s.TriggerUpload()
	s.TriggerUpload() // must not block even though the first is unconsumed

	select {
	case <-s.UploadTrigger:
	default:
		t.Fatal("expected a pending trigger")
	}

	select {
	case <-s.UploadTrigger:
		t.Fatal("second trigger should have collapsed into the first")
	default:
	}
}

func TestSendRecordingCmd_deliversAndWaitsForResult(t *testing.T) {
	s := New()

	go func() {
		cmd := <-s.RecordingCmds
		require.True(t, cmd.Start)
		cmd.Result <- nil
	}()

	err := s.SendRecordingCmd(context.Background(), true)
	require.NoError(t, err)
}

func TestSendRecordingCmd_propagatesHandlerError(t *testing.T) {
	s := New()
	wantErr := context.DeadlineExceeded // any sentinel error works here

	go func() {
		cmd := <-s.RecordingCmds
		cmd.Result <- wantErr
	}()

	err := s.SendRecordingCmd(context.Background(), false)
	require.ErrorIs(t, err, wantErr)
}

func TestSendRecordingCmd_timesOutIfNothingConsumes(t *testing.T) {
	s := New()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := s.SendRecordingCmd(ctx, true)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
