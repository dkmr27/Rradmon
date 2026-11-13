package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkmr27/Rradmon/internal/store"
	"github.com/dkmr27/Rradmon/internal/web"
)

func webFixture(t *testing.T) *httptest.Server {
	t.Helper()
	st := openTestStore(t)
	if _, err := importCapture(st, "../../testdata/rig-corpus1.jsonl", true); err != nil {
		t.Fatal(err)
	}
	ws := web.New(st, nil)
	ws.LoopbackOnly = true
	srv := httptest.NewServer(ws)
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: %d %s", url, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("%s: %v", url, err)
	}
}

func TestWebAPI(t *testing.T) {
	srv := webFixture(t)

	var sm store.Summary
	getJSON(t, srv.URL+"/api/summary", &sm)
	if sm.Packets != 27 || sm.Nodes != 2 || len(sm.Observers) != 1 {
		t.Errorf("summary: %+v", sm)
	}

	// One row per packet: 30 receptions of 27 packets
	var feed []store.PacketRow
	getJSON(t, srv.URL+"/api/feed?before=9999999999999&limit=500", &feed)
	if len(feed) != 27 {
		t.Fatalf("feed rows: %d, want 27", len(feed))
	}
	relayed := 0
	for i, r := range feed {
		if i > 0 && r.T.After(feed[i-1].T) {
			t.Fatal("feed not newest first")
		}
		if len(r.FullHash) != 64 || r.FullHash[:32] != r.Hash {
			t.Errorf("packet %d: full hash %q, truncated %q", r.ID, r.FullHash, r.Hash)
		}
		if r.Receptions == 2 {
			relayed++
			if r.Type != "ANNOUNCE" || r.MaxDelayMs == 0 || r.Hops == nil || *r.Hops != 0 {
				t.Errorf("relayed row should show the original (hops 0) with the relay delay: %+v", r)
			}
			var obs []store.Observation
			getJSON(t, srv.URL+"/api/packets/"+itoa(r.ID)+"/observations", &obs)
			if len(obs) != 2 || obs[0].DelayMs != 0 || obs[1].DelayMs == 0 || *obs[1].Hops != 1 || obs[1].Via == "" {
				t.Errorf("observations of %d: %+v", r.ID, obs)
			}
		}
		if (r.Frames == 2) != (r.Len > 254) {
			t.Errorf("packet %d: frames %d for %d payload bytes", r.ID, r.Frames, r.Len)
		}
	}
	if relayed != 3 {
		t.Errorf("rows with a relayed copy: %d, want 3", relayed)
	}

	// Catch-up returns rows with any activity since: the three announces,
	// whose relayed copies came after the newest packet was first heard.
	var after []store.PacketRow
	getJSON(t, srv.URL+"/api/feed?after="+itoa(feed[0].T.UnixMilli()), &after)
	if len(after) != 3 {
		t.Errorf("feed after: %d rows, want 3", len(after))
	}

	// Find a packet by the hash RNS would log
	var found []store.PacketRow
	getJSON(t, srv.URL+"/api/feed?q="+feed[5].FullHash[:10], &found)
	if len(found) != 1 || found[0].ID != feed[5].ID {
		t.Errorf("hash search: %d rows", len(found))
	}
	getJSON(t, srv.URL+"/api/feed?q=zz", &found)
	if len(found) != 0 {
		t.Errorf("non-hex search returned %d rows", len(found))
	}

	var nodes []store.NodeRow
	getJSON(t, srv.URL+"/api/nodes?q=testing", &nodes)
	if len(nodes) != 1 || nodes[0].Name != "Testing-Rrptr" || nodes[0].Identity != "135a9914ef8e82c36ea3c82bd4689e02" {
		t.Fatalf("nodes search: %+v", nodes)
	}
	var d store.NodeDetail
	getJSON(t, srv.URL+"/api/nodes/"+nodes[0].Identity, &d)
	if len(d.Dests) != 3 || len(d.Announces) != 3 || len(d.Signal) != 3 {
		t.Errorf("node detail: %d dests, %d announces, %d signal", len(d.Dests), len(d.Announces), len(d.Signal))
	}
	for _, a := range d.Announces {
		if a.Copies != 2 || a.MaxDelay == 0 {
			t.Errorf("announce %s: copies %d delay %d", a.App, a.Copies, a.MaxDelay)
		}
	}

	resp, _ := http.Get(srv.URL + "/api/nodes/00")
	if resp.StatusCode != 404 {
		t.Errorf("bad node id: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/")
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "Rradmon") {
		t.Error("index page not served")
	}
}

func TestWebRejectsForeignHost(t *testing.T) {
	srv := webFixture(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/summary", nil)
	req.Host = "evil.example:8080"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
