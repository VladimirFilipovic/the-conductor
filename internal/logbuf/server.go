package logbuf

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// heartbeatInterval keeps an idle stream alive. Both Railway's edge and the
// Next relay in front of it will close a connection that has sent nothing for
// long enough, and a quiet engine is exactly when that happens.
const heartbeatInterval = 20 * time.Second

// defaultReplay is how much history a fresh reader gets when it names no id.
const defaultReplay = 500

// Server exposes the ring over SSE. It is the engine's only listener, and it
// serves reads alone — nothing here can change control-plane state, which is
// why it is safe to run beside the reconcile loop.
type Server struct {
	addr string
	ring *Ring
}

func NewServer(addr string, ring *Ring) *Server {
	return &Server{addr: addr, ring: ring}
}

// Run blocks until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs/stream", s.handleStream)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	// No WriteTimeout: a healthy stream is a response that never finishes.
	srv := &http.Server{Addr: s.addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("logs -> serving", "addr", s.addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("logs: serve: %w", err)
	}
	return nil
}

func (s *Server) handleStream(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	since := resumeID(req)
	tail := defaultReplay
	if n, err := strconv.Atoi(req.URL.Query().Get("tail")); err == nil && n > 0 {
		tail = min(n, DefaultCapacity)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// Nginx-family proxies buffer a response by default, which would turn a
	// live tail into bursts on chunk boundaries.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	backlog, live, cancel := s.ring.SubscribeSince(since, tail)
	defer cancel()

	for _, l := range backlog {
		if !writeEvent(w, l) {
			return
		}
	}
	flusher.Flush()

	beat := time.NewTicker(heartbeatInterval)
	defer beat.Stop()
	for {
		select {
		case <-req.Context().Done():
			return
		case l, ok := <-live:
			// Closed means this reader fell behind and was dropped; ending the
			// response sends it back with its last id rather than leaving it
			// connected to a stream with a hole in it.
			if !ok {
				return
			}
			if !writeEvent(w, l) {
				return
			}
			flusher.Flush()
		case <-beat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// resumeID reads the id to resume after. EventSource sends Last-Event-ID on
// its own reconnects, so honouring it is what makes a dropped connection
// continue; ?since= is the same thing for a caller that isn't EventSource.
func resumeID(req *http.Request) uint64 {
	raw := req.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = req.URL.Query().Get("since")
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func writeEvent(w http.ResponseWriter, l Line) bool {
	_, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", l.ID, l.Text)
	return err == nil
}
