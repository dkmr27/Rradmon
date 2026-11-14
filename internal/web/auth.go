package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Admin login.
//
// Viewing is open (the UI is read-only and listens on 127.0.0.1 by default);
// changes need the admin. The password is set with `rradmon admin
// set-password` and stored only as a bcrypt hash. A login creates an
// in-memory session (lost on restart):
//   - the session id travels in an HttpOnly, SameSite=Strict cookie, so page
//     scripts cannot read it and other sites' requests do not carry it;
//   - every change must also send the session's CSRF token in the
//     X-Rradmon-CSRF header, which other origins cannot read or set;
//   - requests whose Origin is another site are refused outright;
//   - login attempts are rate limited per client address.

const (
	sessionCookie = "rradmon_session"
	csrfHeader    = "X-Rradmon-CSRF"
	sessionTTL    = 12 * time.Hour
	loginBurst    = 5                // attempts
	loginRefill   = 12 * time.Second // one more attempt per this long
)

type session struct {
	csrf    string
	expires time.Time
}

type auth struct {
	mu       sync.Mutex
	sessions map[string]session
	attempts map[string]*loginBucket
}

type loginBucket struct {
	tokens float64
	last   time.Time
}

func newAuth() *auth {
	return &auth{sessions: map[string]session{}, attempts: map[string]*loginBucket{}}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS random source failing is not recoverable
	}
	return hex.EncodeToString(b)
}

// current returns the request's valid session, if any.
func (a *auth) current(r *http.Request) (string, session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[c.Value]
	if !ok || time.Now().After(s.expires) {
		delete(a.sessions, c.Value)
		return "", session{}, false
	}
	return c.Value, s, true
}

// allowLogin is a token bucket per client address.
func (a *auth) allowLogin(r *http.Request, now time.Time) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.attempts[host]
	if !ok {
		b = &loginBucket{tokens: loginBurst, last: now}
		a.attempts[host] = b
	}
	b.tokens = min(loginBurst, b.tokens+now.Sub(b.last).Seconds()/loginRefill.Seconds())
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sameOrigin refuses requests a browser marks as coming from another site.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not sent for same-origin GETs, nor by non-browser clients
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (s *Server) sessionInfo(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.auth.current(r)
	out := map[string]any{"admin": ok, "password_set": s.st.AdminPasswordHash() != ""}
	if ok {
		out["csrf"] = sess.csrf
	}
	s.writeJSON(w, out, nil)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if !s.auth.allowLogin(r, time.Now()) {
		http.Error(w, "too many login attempts; wait a minute", http.StatusTooManyRequests)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	hash := s.st.AdminPasswordHash()
	if hash == "" {
		http.Error(w, "no admin password is set; run: rradmon admin set-password", http.StatusForbidden)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) != nil {
		s.log.Warn("admin login failed", "from", r.RemoteAddr)
		http.Error(w, "wrong password", http.StatusUnauthorized)
		return
	}
	id, csrf := randomToken(), randomToken()
	s.auth.mu.Lock()
	now := time.Now()
	for k, v := range s.auth.sessions { // drop expired sessions
		if now.After(v.expires) {
			delete(s.auth.sessions, k)
		}
	}
	s.auth.sessions[id] = session{csrf: csrf, expires: now.Add(sessionTTL)}
	s.auth.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: int(sessionTTL.Seconds())})
	s.log.Info("admin logged in", "from", r.RemoteAddr)
	s.writeJSON(w, map[string]any{"admin": true, "csrf": csrf}, nil)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if id, _, ok := s.auth.current(r); ok {
		s.auth.mu.Lock()
		delete(s.auth.sessions, id)
		s.auth.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
	s.writeJSON(w, map[string]any{"admin": false}, nil)
}

// admin wraps a handler that changes something: it needs a valid session,
// that session's CSRF token in the header, and a same-origin request.
func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		_, sess, ok := s.auth.current(r)
		if !ok {
			http.Error(w, "admin login required", http.StatusUnauthorized)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(sess.csrf)) != 1 {
			http.Error(w, "missing or wrong CSRF token", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}
