package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dkmr27/Rradmon/internal/store"
	"github.com/dkmr27/Rradmon/internal/web"
)

const testPassword = "correct horse battery"

func adminFixture(t *testing.T, withPassword bool) (*httptest.Server, *store.Store, *http.Client) {
	t.Helper()
	st := openTestStore(t)
	if _, err := importCapture(st, "../../testdata/rig-corpus1.jsonl", true); err != nil {
		t.Fatal(err)
	}
	if withPassword {
		h, _ := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
		if err := st.SetAdminPasswordHash(string(h)); err != nil {
			t.Fatal(err)
		}
	}
	ws := web.New(st, nil)
	ws.LoopbackOnly = true
	srv := httptest.NewServer(ws)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return srv, st, &http.Client{Jar: jar}
}

func call(t *testing.T, c *http.Client, method, url, csrf string, body any, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-Rradmon-CSRF", csrf)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAdminNoPasswordSet(t *testing.T) {
	srv, _, c := adminFixture(t, false)
	_, info := call(t, c, "GET", srv.URL+"/api/session", "", nil, nil)
	if info["admin"] != false || info["password_set"] != false {
		t.Errorf("session: %v", info)
	}
	if code, _ := call(t, c, "POST", srv.URL+"/api/login", "", map[string]string{"password": "anything at all"}, nil); code != 403 {
		t.Errorf("login with no password set: %d, want 403", code)
	}
}

func TestAdminLoginRateLimit(t *testing.T) {
	srv, _, c := adminFixture(t, true)
	codes := []int{}
	for i := 0; i < 7; i++ {
		code, _ := call(t, c, "POST", srv.URL+"/api/login", "", map[string]string{"password": "wrong password!"}, nil)
		codes = append(codes, code)
	}
	if codes[0] != 401 || codes[4] != 401 || codes[5] != 429 || codes[6] != 429 {
		t.Errorf("codes %v: want five 401 then 429", codes)
	}
}

func TestAdminFlow(t *testing.T) {
	srv, st, c := adminFixture(t, true)
	u := srv.URL

	// Nothing works before login
	if code, _ := call(t, c, "GET", u+"/api/admin/settings", "", nil, nil); code != 401 {
		t.Fatalf("settings before login: %d", code)
	}
	code, out := call(t, c, "POST", u+"/api/login", "", map[string]string{"password": testPassword}, nil)
	csrf, _ := out["csrf"].(string)
	if code != 200 || len(csrf) != 64 {
		t.Fatalf("login: %d %v", code, out)
	}
	if _, info := call(t, c, "GET", u+"/api/session", "", nil, nil); info["admin"] != true || info["csrf"] != csrf {
		t.Errorf("session after login: %v", info)
	}

	// Changes need the CSRF token, and must not come from another origin
	change := map[string]string{"retain.receptions_days": "21"}
	if code, _ := call(t, c, "PUT", u+"/api/admin/settings", "", change, nil); code != 403 {
		t.Errorf("no CSRF: %d, want 403", code)
	}
	if code, _ := call(t, c, "PUT", u+"/api/admin/settings", "0123", change, nil); code != 403 {
		t.Errorf("wrong CSRF: %d, want 403", code)
	}
	if code, _ := call(t, c, "PUT", u+"/api/admin/settings", csrf, change, map[string]string{"Origin": "http://evil.example"}); code != 403 {
		t.Errorf("cross origin: %d, want 403", code)
	}
	if st.Setting("retain.receptions_days") != "14" {
		t.Fatal("a refused request changed a setting")
	}
	if code, _ := call(t, c, "PUT", u+"/api/admin/settings", csrf, change, nil); code != 200 || st.Setting("retain.receptions_days") != "21" {
		t.Errorf("valid change: %d, value %s", code, st.Setting("retain.receptions_days"))
	}

	// One bad value: nothing saved
	bad := map[string]string{"retain.announces_days": "30", "ingest.window_other": "3 weeks"}
	if code, _ := call(t, c, "PUT", u+"/api/admin/settings", csrf, bad, nil); code != 400 || st.Setting("retain.announces_days") != "60" {
		t.Errorf("bad value: %d, announces_days %s", code, st.Setting("retain.announces_days"))
	}
	if code, _ := call(t, c, "PUT", u+"/api/admin/settings", csrf, map[string]string{"admin.password_hash": "x"}, nil); code != 400 {
		t.Errorf("setting the password hash through settings: %d, want 400", code)
	}

	// Pin rnsd (no announced position) by hand; the map shows it
	rnsd := "24de22aa3a4a814c"
	var ident string
	st.DB.QueryRow(`SELECT lower(hex(tx_identity)) FROM receptions WHERE lower(hex(tx_identity)) LIKE ? LIMIT 1`, rnsd[:8]+"%").Scan(&ident)
	if code, _ := call(t, c, "PUT", u+"/api/admin/pins/"+ident, csrf, map[string]any{"lat": -10.5, "lon": 120.25, "note": "bench"}, nil); code != 200 {
		t.Fatalf("set pin: %d", code)
	}
	if code, _ := call(t, c, "PUT", u+"/api/admin/pins/"+ident, csrf, map[string]any{"lat": 95, "lon": 0}, nil); code != 400 {
		t.Errorf("bad latitude: %d, want 400", code)
	}
	md, _ := st.Map(time.Now())
	found := false
	for _, n := range md.Nodes {
		if n.Identity == ident {
			found = n.PosSource == "manual" && n.Lat == -10.5 && strings.Contains(n.Info, "bench")
		}
	}
	if !found {
		t.Errorf("pinned node not on the map as manual: %+v", md.Nodes)
	}
	if code, _ := call(t, c, "DELETE", u+"/api/admin/pins/"+ident, csrf, nil, nil); code != 200 {
		t.Errorf("clear pin: %d", code)
	}

	// Name it by hand; the node list shows the name, and the map the pin
	// had no effect on it
	if code, _ := call(t, c, "PUT", u+"/api/admin/names/"+ident, csrf, map[string]any{"name": "rnsd on the PC"}, nil); code != 200 {
		t.Fatalf("set name: %d", code)
	}
	if code, _ := call(t, c, "PUT", u+"/api/admin/names/"+ident, csrf, map[string]any{"name": "badname"}, nil); code != 400 {
		t.Errorf("control character in name: %d, want 400", code)
	}
	if nodes, _ := st.Nodes("rnsd on the pc", time.Now()); len(nodes) != 1 || nodes[0].Name != "rnsd on the PC" || !nodes[0].NameManual {
		t.Errorf("named node: %+v", nodes)
	}
	if code, _ := call(t, c, "DELETE", u+"/api/admin/names/"+ident, csrf, nil, nil); code != 200 {
		t.Errorf("clear name: %d", code)
	}
	if nodes, _ := st.Nodes("rnsd on the pc", time.Now()); len(nodes) != 0 {
		t.Errorf("name still there after clearing: %+v", nodes)
	}

	// Strip payloads keeps announces
	code, out = call(t, c, "POST", u+"/api/admin/strip-payloads", csrf, nil, nil)
	var left int
	st.DB.QueryRow(`SELECT COUNT(*) FROM packets WHERE payload IS NOT NULL AND pkt_type <> 1`).Scan(&left)
	if code != 200 || out["stripped"].(float64) != 24 || left != 0 {
		t.Errorf("strip: %d %v, %d left", code, out, left)
	}

	// Logout ends the session
	call(t, c, "POST", u+"/api/logout", "", nil, nil)
	if code, _ := call(t, c, "PUT", u+"/api/admin/settings", csrf, change, nil); code != 401 {
		t.Errorf("after logout: %d, want 401", code)
	}
	if code, _ := call(t, c, "PUT", u+"/api/admin/names/"+ident, csrf, map[string]any{"name": "x"}, nil); code != 401 {
		t.Errorf("name after logout: %d, want 401", code)
	}
}

func TestCheckPassword(t *testing.T) {
	if _, err := checkPassword("short"); err == nil {
		t.Error("accepted a short password")
	}
	if _, err := checkPassword(strings.Repeat("x", 80)); err == nil {
		t.Error("accepted a password over bcrypt's 72-byte limit")
	}
	if _, err := checkPassword(testPassword); err != nil {
		t.Error(err)
	}
}
