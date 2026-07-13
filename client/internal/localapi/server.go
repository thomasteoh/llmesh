// Package localapi exposes a local OpenAI-compatible HTTP endpoint that routes
// requests directly to the appropriate llama.cpp backend, bypassing the router.
// Each active request occupies an active-job slot in the shared stats counter
// so it is visible in the status line alongside router-dispatched jobs.
package localapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	clientPkg "llmesh/client"
	"llmesh/client/internal/stats"
	"llmesh/pkg/wsclient"
)

const maxBodyBytes = 10 << 20 // 10 MiB

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Server serves the local API endpoint.
type Server struct {
	cfg  *clientPkg.Config
	st   *stats.Stats
	pool *wsclient.SlotPool
}

// New creates a Server. pool is the shared slot pool from the wsclient connection;
// local requests acquire from it with priority over router-dispatched jobs.
// Call Run to start listening.
func New(cfg *clientPkg.Config, st *stats.Stats, pool *wsclient.SlotPool) *Server {
	return &Server{cfg: cfg, st: st, pool: pool}
}

// Run starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", s.handleModels)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Info("local API listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("local API: %w", err)
	}
	return nil
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
		return
	}

	// MaxBytesReader (not LimitReader) so an oversized body is rejected with a
	// clear error rather than silently truncated into invalid JSON.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			fmt.Fprint(w, `{"error":"request body too large"}`)
			return
		}
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}

	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	endpoint := s.cfg.EndpointFor(envelope.Model)
	if endpoint == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":"model %q not available on this client"}`, envelope.Model)
		return
	}

	target, err := url.Parse(endpoint)
	if err != nil {
		http.Error(w, `{"error":"invalid backend endpoint"}`, http.StatusInternalServerError)
		return
	}

	if !s.pool.AcquireLocal(r.Context()) {
		return // client disconnected while waiting for a slot
	}
	s.st.IncrActive()
	defer func() {
		s.pool.Release()
		s.st.DecrActive()
		s.st.IncrDone()
	}()

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = "/v1/chat/completions"
			req.URL.RawQuery = ""
			req.Host = target.Host
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.Header.Del("Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // client disconnected
			}
			log.Error("local API: proxy error", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `{"error":"backend unavailable: %s"}`, err.Error())
		},
	}

	proxy.ServeHTTP(w, r)
}

// authorized reports whether the request may use the local endpoint. When no
// local_api_token is configured the endpoint is open (intended for loopback
// binds); otherwise a matching bearer token is required, compared in constant
// time.
func (s *Server) authorized(r *http.Request) bool {
	want := s.cfg.LocalAPIToken
	if want == "" {
		return true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(want)) == 1
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
		return
	}

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	type response struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}

	names := s.cfg.AvailableModels()
	entries := make([]modelEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, modelEntry{
			ID:      name,
			Object:  "model",
			OwnedBy: "local",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response{Object: "list", Data: entries})
}
