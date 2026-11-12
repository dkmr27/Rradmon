// Package web serves Rradmon's read-only UI and JSON API.
//
// The UI is plain HTML/JS embedded in the binary (no build step, works
// offline). Live updates use server-sent events: the stream only says "new
// data at time T", and the page fetches /api/feed?after=… to catch up, so a
// slow or reconnecting browser never misses rows.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dkmr27/Rradmon/internal/store"
)

//go:embed static
var static embed.FS

// Server is the HTTP handler.
type Server struct {
	st  *store.Store
	log *slog.Logger
	mux *http.ServeMux

	// LoopbackOnly rejects requests whose Host is not a loopback name. Set it
	// when listening on 127.0.0.1 (the default).
	LoopbackOnly bool

	mu   sync.Mutex
	subs map[chan time.Time]struct{}
}

// New builds the handler.
func New(st *store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{st: st, log: log, mux: http.NewServeMux(), subs: map[chan time.Time]struct{}{}}
	sub, _ := fs.Sub(static, "static")
	files := http.FileServer(http.FS(sub))
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Embedded files have no modification time; make the browser
		// revalidate so a new binary's UI shows up without a hard refresh.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	s.mux.HandleFunc("GET /api/summary", s.summary)
	s.mux.HandleFunc("GET /api/feed", s.feed)
	s.mux.HandleFunc("GET /api/nodes", s.nodes)
	s.mux.HandleFunc("GET /api/nodes/{id}", s.node)
	s.mux.HandleFunc("GET /api/channel/{observer}", s.channel)
	s.mux.HandleFunc("GET /api/stream", s.stream)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read-only API on a local port: refuse cross-site use of it from other
	// pages the browser has open.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
	if s.LoopbackOnly && !loopbackHost(r.Host) {
		// Blocks DNS-rebinding: a hostile page cannot read the local API
		// through a name that resolves to 127.0.0.1.
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func loopbackHost(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	switch strings.Trim(h, "[]") {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// Notify tells live clients that data up to t has been stored. Safe to call
// from the ingest goroutine; never blocks.
func (s *Server) Notify(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- t:
		default: // client is behind; it will catch up on the next one
		}
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, v any, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("api", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	sm, err := s.st.Summary(time.Now())
	s.writeJSON(w, sm, err)
}

func intParam(r *http.Request, name string, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, max)
}

func timeParam(r *http.Request, name string) (time.Time, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

// feed: ?before=<ms> pages back (newest first); ?after=<ms> catches up
// (oldest first).
func (s *Server) feed(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 100, 500)
	if after, ok := timeParam(r, "after"); ok {
		rows, err := s.st.FeedAfter(after, limit)
		s.writeJSON(w, nonNil(rows), err)
		return
	}
	before, ok := timeParam(r, "before")
	if !ok {
		before = time.Now().Add(time.Hour)
	}
	rows, err := s.st.Feed(before, limit)
	s.writeJSON(w, nonNil(rows), err)
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.Nodes(r.URL.Query().Get("q"), time.Now())
	s.writeJSON(w, nonNil(rows), err)
}

func (s *Server) node(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Node(r.PathValue("id"), time.Now())
	s.writeJSON(w, d, err)
}

func (s *Server) channel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("observer"), 10, 64)
	if err != nil {
		http.Error(w, "bad observer", http.StatusBadRequest)
		return
	}
	hours := intParam(r, "hours", 1, 24*30)
	pts, err := s.st.Channel(id, time.Now().Add(-time.Duration(hours)*time.Hour), 600)
	s.writeJSON(w, nonNil(pts), err)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan time.Time, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte("retry: 3000\n\n"))
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case t := <-ch:
			w.Write([]byte("event: data\ndata: " + strconv.FormatInt(t.UnixMilli(), 10) + "\n\n"))
			fl.Flush()
		case <-ping.C:
			w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
