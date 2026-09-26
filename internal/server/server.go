// Package server exposes the Action URL ingest endpoint that phones call and
// a read-only JSON API over the stored data.
package server

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shambles07/grandstream-actionurl-server/internal/actionurl"
	"github.com/shambles07/grandstream-actionurl-server/internal/store"
)

// Config configures a Server.
type Config struct {
	Addr string
	// Listener, if non-nil, is used instead of listening on Addr (for
	// example a socket inherited from systemd).
	Listener net.Listener
	// IngestToken, if set, must be present as token=<value> on every
	// Action URL request.
	IngestToken string
	// APIToken, if set, must be sent as "Authorization: Bearer <value>" on
	// every /api request.
	APIToken string
	// TLSCert and TLSKey enable HTTPS when both are set.
	TLSCert, TLSKey string
}

// Server is the HTTP front end.
type Server struct {
	cfg   Config
	store *store.Store
	log   *slog.Logger
	hub   *hub
	http  *http.Server
}

// New builds a Server.
func New(cfg Config, st *store.Store, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, store: st, log: log, hub: newHub()}
	s.http = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// Handler returns the server's HTTP handler (useful for tests).
func (s *Server) Handler() http.Handler { return s.http.Handler }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+actionurl.IngestPathPrefix, s.handleIngest)
	mux.HandleFunc("POST "+actionurl.IngestPathPrefix, s.handleIngest)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})

	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/catalog", s.handleCatalog)
	api.HandleFunc("GET /api/v1/phones", s.handleListPhones)
	api.HandleFunc("GET /api/v1/phones/{mac}", s.handleGetPhone)
	api.HandleFunc("GET /api/v1/phones/{mac}/events", s.handleListEvents)
	api.HandleFunc("GET /api/v1/phones/{mac}/calls", s.handleListCalls)
	api.HandleFunc("GET /api/v1/events", s.handleListEvents)
	api.HandleFunc("GET /api/v1/events/stats", s.handleEventStats)
	api.HandleFunc("GET /api/v1/events/stream", s.handleStream)
	api.HandleFunc("GET /api/v1/calls", s.handleListCalls)
	mux.Handle("/api/", s.requireAPIToken(api))
	return mux
}

// Serve listens on cfg.Listener, or cfg.Addr if that is nil, until ctx is
// cancelled.
func (s *Server) Serve(ctx context.Context) error {
	ln := s.cfg.Listener
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", s.cfg.Addr); err != nil {
			return err
		}
	}
	if s.cfg.TLSCert != "" && s.cfg.TLSKey != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
		if err != nil {
			ln.Close()
			return fmt.Errorf("load TLS keypair: %w", err)
		}
		ln = tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{cert},
			// Many deployed phones only speak TLS 1.2.
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		})
	}
	ln = lenientListener{ln}

	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(ln) }()
	s.log.Info("listening", "addr", ln.Addr().String(), "tls", s.cfg.TLSCert != "")

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	s.hub.close()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.http.Shutdown(shutCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) requireAPIToken(next http.Handler) http.Handler {
	if s.cfg.APIToken == "" {
		return next
	}
	want := []byte("Bearer " + s.cfg.APIToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"events":    actionurl.Events,
		"variables": actionurl.Variables,
	})
}

func (s *Server) handleListPhones(w http.ResponseWriter, r *http.Request) {
	phones, err := s.store.ListPhones(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"phones": phones})
}

func (s *Server) handleGetPhone(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetPhone(r.Context(), normalizeMAC(r.PathValue("mac")))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "phone not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.EventFilter{
		MAC:    normalizeMAC(q.Get("mac")),
		Event:  q.Get("event"),
		CallID: q.Get("call_id"),
	}
	if m := r.PathValue("mac"); m != "" {
		f.MAC = normalizeMAC(m)
	}
	if f.Event != "" {
		if _, ok := actionurl.LookupEvent(f.Event); !ok {
			writeError(w, http.StatusBadRequest, "unknown event "+strconv.Quote(f.Event))
			return
		}
	}
	var err error
	if f.Since, err = parseTimeParam(q.Get("since")); err != nil {
		writeError(w, http.StatusBadRequest, "since: "+err.Error())
		return
	}
	if f.Until, err = parseTimeParam(q.Get("until")); err != nil {
		writeError(w, http.StatusBadRequest, "until: "+err.Error())
		return
	}
	f.BeforeID, _ = strconv.ParseInt(q.Get("before_id"), 10, 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))

	events, err := s.store.ListEvents(r.Context(), f)
	if err != nil {
		s.internalError(w, err)
		return
	}
	resp := map[string]any{"events": events}
	if n := len(events); n > 0 {
		resp["next_before_id"] = events[n-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleEventStats(w http.ResponseWriter, r *http.Request) {
	since, err := parseTimeParam(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "since: "+err.Error())
		return
	}
	counts, err := s.store.CountEvents(r.Context(), since)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"counts": counts})
}

func (s *Server) handleListCalls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.CallFilter{
		MAC:    normalizeMAC(q.Get("mac")),
		CallID: q.Get("call_id"),
		State:  q.Get("state"),
	}
	if m := r.PathValue("mac"); m != "" {
		f.MAC = normalizeMAC(m)
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	calls, err := s.store.ListCalls(r.Context(), f)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": calls})
}

// parseTimeParam accepts RFC 3339 timestamps or a Go duration meaning "that
// long ago" (e.g. "24h").
func parseTimeParam(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	d, err := ParseDuration(v)
	if err != nil {
		return time.Time{}, fmt.Errorf("want RFC 3339 time or duration like 24h or 7d")
	}
	return time.Now().Add(-d), nil
}

// ParseDuration extends time.ParseDuration with a "d" (24h) suffix.
func ParseDuration(v string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(v)
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.log.Error("api", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
