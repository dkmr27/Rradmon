package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/dkmr27/Rradmon/internal/store"
)

// Admin API: settings, manual node positions, maintenance. Every handler is
// wrapped by Server.admin (session + CSRF + same origin).

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.st.EditableSettings(), nil)
}

// adminSaveSettings takes {"key": "value", ...}; nothing is saved unless every
// value is valid.
func (s *Server) adminSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	clean := map[string]string{}
	for k, v := range body {
		nv, err := store.ValidateSetting(k, v)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		clean[k] = nv
	}
	for k, v := range clean {
		if err := s.st.SetSetting(k, v); err != nil {
			s.writeJSON(w, nil, err)
			return
		}
	}
	s.log.Info("settings changed by admin", "count", len(clean))
	s.writeJSON(w, s.st.EditableSettings(), nil)
}

func (s *Server) adminSetPin(w http.ResponseWriter, r *http.Request) {
	var p store.Pin
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	err := s.st.SetPin(r.PathValue("id"), p)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.writeJSON(w, map[string]bool{"ok": err == nil}, err)
}

func (s *Server) adminClearPin(w http.ResponseWriter, r *http.Request) {
	err := s.st.ClearPin(r.PathValue("id"))
	s.writeJSON(w, map[string]bool{"ok": err == nil}, err)
}

func (s *Server) adminSetObserverPin(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var p store.Pin
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	err = s.st.SetObserverPin(id, p)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.writeJSON(w, map[string]bool{"ok": err == nil}, err)
}

func (s *Server) adminClearObserverPin(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.st.ClearObserverPin(id)
	s.writeJSON(w, map[string]bool{"ok": err == nil}, err)
}

func (s *Server) adminForgetObserver(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ext, err := s.st.ForgetObserver(id)
	if err == nil {
		s.log.Info("observer forgotten", "observer", ext)
	}
	s.writeJSON(w, map[string]bool{"ok": err == nil}, err)
}

func (s *Server) adminStripPayloads(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.StripPayloads()
	if err == nil {
		s.log.Info("stored payloads stripped by admin", "packets", n)
	}
	s.writeJSON(w, map[string]int64{"stripped": n}, err)
}
