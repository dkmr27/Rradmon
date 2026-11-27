package ingest

import (
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/store"
)

// rrcd's announce app data: cbor2.dumps({"proto": "rrc", "v": 1, "hub": name})
func rrcHubData(name string) []byte {
	b := []byte{0xa3, 0x65, 'p', 'r', 'o', 't', 'o', 0x63, 'r', 'r', 'c', 0x61, 'v', 0x01, 0x63, 'h', 'u', 'b'}
	return append(append(b, byte(0x60+len(name))), name...)
}

// An rrcd hub's announce names the node and reads in the live feed; a hub
// stored before rrc.hub was known is put right when the database is opened.
func TestRRCHubAnnounce(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	in, err := New(st, radio, nil)
	if err != nil {
		t.Fatal(err)
	}
	hub, ident := signedAnnounce(t, "rrc.hub", rrcHubData("Bench Hub"))
	must(t, in.Frame("A", "test", frame(0, hub, -80), true))

	check := func(st *store.Store, when string) {
		t.Helper()
		var app, name, info string
		err := st.DB.QueryRow(`SELECT COALESCE(app_name, ''), COALESCE(display_name, ''), COALESCE(info, '')
			FROM destinations WHERE identity_hash = ?`, ident).Scan(&app, &name, &info)
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if app != "rrc.hub" || name != "Bench Hub" || info != `RRC hub · "Bench Hub" · rrc v1` {
			t.Errorf("%s: app %q, name %q, info %q", when, app, name, info)
		}
		rows, err := st.PacketFeed(t0.Add(time.Hour), time.Time{}, store.FeedFilter{}, 10)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: feed %d rows, %v", when, len(rows), err)
		}
		if r := rows[0]; r.AppName != "rrc.hub" || r.DestName != "Bench Hub" || r.Info != `RRC hub · "Bench Hub" · rrc v1` {
			t.Errorf("%s: feed row app %q, dest name %q, info %q", when, r.AppName, r.DestName, r.Info)
		}
	}
	check(st, "as heard")

	// What a database written before this change holds for the hub, at
	// schema version 9
	_, err = st.DB.Exec(`UPDATE destinations SET app_name = NULL, display_name = NULL,
		info = 'Announce · 40 bytes of app data' WHERE identity_hash = ?`, ident)
	must(t, err)
	_, err = st.DB.Exec(`PRAGMA user_version = 9`)
	must(t, err)
	st.Close()

	st, err = store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	check(st, "after the upgrade")
}
