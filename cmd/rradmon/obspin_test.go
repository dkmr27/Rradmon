package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/store"
)

func TestObserverPositions(t *testing.T) {
	srv, st, c := adminFixture(t, true)
	u := srv.URL
	_, out := call(t, c, "POST", u+"/api/login", "", map[string]string{"password": testPassword}, nil)
	csrf := out["csrf"].(string)

	// A reported 0,0 is no position
	zero := 0.0
	if err := st.ObserverStatus("obs-a", "mqtt", time.Now(), store.ObserverState{Online: true, Lat: &zero, Lon: &zero}); err != nil {
		t.Fatal(err)
	}
	md, _ := st.Map(time.Now())
	for _, o := range md.Observers {
		if o.ExtID == "obs-a" {
			t.Fatalf("observer reporting 0,0 placed on the map: %+v", o)
		}
	}
	id, _ := st.Observer("obs-a", "mqtt", time.Now())
	path := fmt.Sprintf("%s/api/admin/observer-pins/%d", u, id)

	if code, _ := call(t, c, "PUT", path, csrf, map[string]any{"lat": 0, "lon": 0}, nil); code != 400 {
		t.Errorf("pin at 0,0: %d, want 400", code)
	}
	if code, _ := call(t, c, "PUT", path, "", map[string]any{"lat": -10.5, "lon": 120.25}, nil); code != 403 {
		t.Errorf("no CSRF: %d, want 403", code)
	}
	if code, _ := call(t, c, "PUT", u+"/api/admin/observer-pins/99999", csrf, map[string]any{"lat": -10.5, "lon": 120.25}, nil); code != 404 {
		t.Errorf("unknown observer: %d, want 404", code)
	}
	if code, _ := call(t, c, "PUT", path, csrf, map[string]any{"lat": -10.5, "lon": 120.25, "note": "roof"}, nil); code != 200 {
		t.Fatalf("set observer pin: %d", code)
	}
	// A later reported position does not move a hand-set one
	lat, lon := 1.0, 2.0
	st.ObserverStatus("obs-a", "mqtt", time.Now(), store.ObserverState{Online: true, Lat: &lat, Lon: &lon})
	md, _ = st.Map(time.Now())
	var got *store.MapObserver
	for i := range md.Observers {
		if md.Observers[i].ExtID == "obs-a" {
			got = &md.Observers[i]
		}
	}
	if got == nil || got.Lat != -10.5 || got.PosSource != "manual" || got.Note != "roof" || got.ID != id {
		t.Fatalf("map observer %+v", got)
	}
	// Cleared: back to the reported one
	if code, _ := call(t, c, "DELETE", path, csrf, nil, nil); code != 200 {
		t.Fatalf("clear: %d", code)
	}
	md, _ = st.Map(time.Now())
	for _, o := range md.Observers {
		if o.ExtID == "obs-a" && (o.Lat != 1 || o.PosSource != "reported") {
			t.Errorf("after clear %+v", o)
		}
	}
}
