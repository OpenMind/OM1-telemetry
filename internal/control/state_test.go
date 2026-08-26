package control

import (
	"testing"

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

func TestTryClaimDir_secondClaimFailsUntilReleased(t *testing.T) {
	s := New()
	require.True(t, s.TryClaimDir("/a"), "first claim on a free dir must succeed")
	require.False(t, s.TryClaimDir("/a"), "a second claim on an already-held dir must fail")

	s.ReleaseDir("/a")
	require.True(t, s.TryClaimDir("/a"), "the dir must be claimable again once released")
}

func TestTryClaimDir_independentPerDir(t *testing.T) {
	s := New()
	require.True(t, s.TryClaimDir("/a"))
	require.True(t, s.TryClaimDir("/b"), "claiming one dir must not affect another")
}
