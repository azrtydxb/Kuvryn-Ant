// Package server provides the HTTP lifecycle and dependency health endpoints.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type Pinger interface{ Ping(context.Context) error }

type Server struct {
	http            *http.Server
	stopping        atomic.Bool
	shutdownTimeout time.Duration
}

func New(db Pinger, maxRequests int, shutdownTimeout time.Duration, logger *slog.Logger) *Server {
	s := &Server{shutdownTimeout: shutdownTimeout}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { status(w, http.StatusOK, "alive") })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if s.stopping.Load() {
			status(w, http.StatusServiceUnavailable, "not_ready")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			status(w, http.StatusServiceUnavailable, "not_ready")
			return
		}
		status(w, http.StatusOK, "ready")
	})
	slots := make(chan struct{}, maxRequests)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.stopping.Load() {
			status(w, http.StatusServiceUnavailable, "stopping")
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "1")
			status(w, http.StatusServiceUnavailable, "busy")
			return
		}
		mux.ServeHTTP(w, r)
	})
	s.http = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError)}
	return s
}

func status(w http.ResponseWriter, code int, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte("{\"status\":\"" + value + "\"}\n"))
}

// Serve drains accepted requests on cancellation, then forcibly closes overdue
// connections. It returns only after the HTTP serving goroutine has exited.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	s.http.BaseContext = func(net.Listener) context.Context { return requests }
	done := make(chan error, 1)
	go func() { done <- s.http.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.stopping.Store(true)
		drain, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		err := s.http.Shutdown(drain)
		if err != nil {
			cancelRequests()
			_ = s.http.Close()
		}
		<-done
		return err
	}
}
