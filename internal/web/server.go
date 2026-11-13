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

	// Counters, if set, returns live per-observer MQTT counters since this
	// server started (they are not stored).
	Counters func() map[string]Counters

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
	s.mux.HandleFunc("GET /api/observers", s.observers)
	s.mux.HandleFunc("GET /api/map", s.mapData)
	s.mux.HandleFunc("GET /api/packets/{id}/observations", s.packetObservations)
	s.mux.HandleFunc("GET /api/orphans/{id}/observations", s.orphanObservations)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read-only API on a local port: refuse cross-site use of it from other
	// pages the browser has open.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	// Map tiles come from OpenStreetMap; the page can turn them off
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https://tile.openstreetmap.org")
	// Tile requests carry no referrer path beyond the origin
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
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

// feed: one row per packet (and per unmatched orphan group).
// ?before=<ms> pages back, newest first; ?after=<ms> returns rows with any
// activity since, oldest first; ?q=<hex> narrows to a packet hash prefix.
func (s *Server) feed(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 100, 500)
	q := r.URL.Query().Get("q")
	after, _ := timeParam(r, "after")
	before, ok := timeParam(r, "before")
	if !ok {
		before = time.Now().Add(time.Hour)
	}
	rows, err := s.st.PacketFeed(before, after, q, limit)
	s.writeJSON(w, nonNil(rows), err)
}

func (s *Server) mapData(w http.ResponseWriter, r *http.Request) {
	md, err := s.st.Map(time.Now())
	md.Nodes, md.Observers = nonNil(md.Nodes), nonNil(md.Observers)
	s.writeJSON(w, md, err)
}

func idParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) packetObservations(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	obs, err := s.st.PacketObservations(id)
	s.writeJSON(w, nonNil(obs), err)
}

func (s *Server) orphanObservations(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	obs, err := s.st.OrphanObservations(id)
	s.writeJSON(w, nonNil(obs), err)
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

// Counters are an MQTT observer's message counts since the server started.
type Counters struct {
	Accepted    uint64 `json:"accepted"`
	Invalid     uint64 `json:"invalid"`
	RateLimited uint64 `json:"rate_limited"`
	SeqGaps     uint64 `json:"seq_gaps"`
}

type observerJSON struct {
	store.ObserverView
	MQTT *Counters `json:"mqtt,omitempty"`
}

func (s *Server) observers(w http.ResponseWriter, r *http.Request) {
	views, err := s.st.Observers(time.Now())
	if err != nil {
		s.writeJSON(w, nil, err)
		return
	}
	var counters map[string]Counters
	if s.Counters != nil {
		counters = s.Counters()
	}
	out := make([]observerJSON, 0, len(views))
	for _, v := range views {
		o := observerJSON{ObserverView: v}
		if c, ok := counters[v.ExtID]; ok {
			o.MQTT = &c
		}
		out = append(out, o)
	}
	s.writeJSON(w, out, nil)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
