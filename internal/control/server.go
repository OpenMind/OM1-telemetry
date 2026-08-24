package control

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// commandTimeout bounds how long an HTTP handler waits for main's event
// loop to act on a recording start/stop request, so a wedged recording
// loop fails the HTTP call instead of hanging it forever.
const commandTimeout = 30 * time.Second

// Status is the JSON body for GET /status and for every mutating endpoint's
// response, so a caller always sees the resulting state without a
// follow-up request.
type Status struct {
	Recording       bool   `json:"recording"`
	Uploading       bool   `json:"uploading"`
	CurrentSession  string `json:"current_session"`
	RecordingsBytes int64  `json:"recordings_bytes"`
	MaxBytes        int64  `json:"max_bytes"`
	PendingUploads  int    `json:"pending_uploads"`
}

func (s *State) status() Status {
	st := Status{
		Recording: s.Recording(),
		Uploading: s.Uploading(),
	}
	if s.Extra != nil {
		extra := s.Extra()
		st.CurrentSession = extra.CurrentSession
		st.RecordingsBytes = extra.RecordingsBytes
		st.MaxBytes = extra.MaxBytes
		st.PendingUploads = extra.PendingUploads
	}
	return st
}

// Handler builds the control-plane HTTP handler. Split from Serve so tests
// can exercise it with httptest.NewServer without binding a real port.
func (s *State) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /recording/start", s.handleRecording(true))
	mux.HandleFunc("POST /recording/stop", s.handleRecording(false))
	mux.HandleFunc("POST /upload/start", s.handleUploadStart)
	mux.HandleFunc("POST /upload/stop", s.handleUploadStop)
	return mux
}

// Serve starts the control HTTP server on addr and blocks until ctx is
// canceled, then shuts it down gracefully. addr is expected to be
// loopback-only (e.g. 127.0.0.1:9191) -- the container already runs with
// host networking, so this is reachable from the Thor host and nowhere
// else; there is no authentication.
func Serve(ctx context.Context, addr string, state *State) error {
	srv := &http.Server{Addr: addr, Handler: state.Handler()}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func (s *State) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.status())
}

func (s *State) handleRecording(start bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), commandTimeout)
		defer cancel()

		if err := s.SendRecordingCmd(ctx, start); err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, context.DeadlineExceeded) {
				code = http.StatusGatewayTimeout
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.status())
	}
}

func (s *State) handleUploadStart(w http.ResponseWriter, r *http.Request) {
	s.SetUploading(true)
	s.TriggerUpload()
	writeJSON(w, http.StatusOK, s.status())
}

func (s *State) handleUploadStop(w http.ResponseWriter, r *http.Request) {
	s.SetUploading(false)
	writeJSON(w, http.StatusOK, s.status())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("control: failed to encode response", "err", err)
	}
}
