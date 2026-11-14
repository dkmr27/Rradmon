package main

import (
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/store"
)

// The broker keeps the last will of every observer that ever connected. A
// server that never saw an observer online must not list it from that alone.
func TestOfflineStatusOfUnknownObserver(t *testing.T) {
	_, st, _ := adminFixture(t, false)
	now := time.Now()
	listed := func(id string) *store.ObserverView {
		obs, err := st.Observers(now)
		if err != nil {
			t.Fatal(err)
		}
		for i := range obs {
			if obs[i].ExtID == id {
				return &obs[i]
			}
		}
		return nil
	}
	if err := st.ObserverStatus("obs-gone", "mqtt", now, store.ObserverState{Online: false}); err != nil {
		t.Fatal(err)
	}
	if o := listed("obs-gone"); o != nil {
		t.Fatalf("offline status created an observer: %+v", o)
	}
	// Online creates it; a later offline status marks it offline
	if err := st.ObserverStatus("obs-b", "mqtt", now, store.ObserverState{Online: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.ObserverStatus("obs-b", "mqtt", now.Add(time.Minute), store.ObserverState{Online: false}); err != nil {
		t.Fatal(err)
	}
	if o := listed("obs-b"); o == nil || o.State != "offline" {
		t.Fatalf("obs-b: %+v", o)
	}
}

func TestObserverKnown(t *testing.T) {
	_, st, _ := adminFixture(t, false)
	if k, err := st.ObserverKnown("obs-k"); err != nil || k {
		t.Fatalf("unknown: %v %v", k, err)
	}
	if err := st.ObserverStatus("obs-k", "mqtt", time.Now(), store.ObserverState{Online: true}); err != nil {
		t.Fatal(err)
	}
	if k, _ := st.ObserverKnown("obs-k"); !k {
		t.Error("known observer reported unknown")
	}
}
