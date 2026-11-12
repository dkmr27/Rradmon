package main

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/dkmr27/Rradmon/internal/ingest"
	"github.com/dkmr27/Rradmon/internal/mqtt"
	"github.com/dkmr27/Rradmon/internal/reassembly"
	"github.com/dkmr27/Rradmon/internal/store"
)

// mqttIngest feeds validated MQTT messages into the ingestor. Observers on a
// different radio network are ignored (one network per server for now).
type mqttIngest struct {
	in      *ingest.Ingestor
	st      *store.Store
	network string
	log     *slog.Logger
	other   atomic.Uint64
}

func (h *mqttIngest) wrongNetwork(network, observer string) bool {
	if network == h.network {
		return false
	}
	if n := h.other.Add(1); n == 1 || n%1000 == 0 {
		h.log.Warn("ignoring observer on another radio network", "observer", observer, "network", network, "count", n)
	}
	return true
}

func (h *mqttIngest) Rx(network, observer string, m *mqtt.Rx, tRx, tObs time.Time) {
	if h.wrongNetwork(network, observer) {
		return
	}
	f := reassembly.Frame{T: tRx, Data: m.Frame}
	if m.RSSI != nil && m.SNR != nil {
		f.RSSI, f.SNR, f.HasSignal = *m.RSSI, *m.SNR, true
	}
	if err := h.in.Frame(observer, "mqtt", f, m.IsRaw()); err != nil {
		h.log.Error("ingest failed", "observer", observer, "err", err)
	}
}

func (h *mqttIngest) Chan(network, observer string, m *mqtt.Chan, tRx, tObs time.Time) {
	if h.wrongNetwork(network, observer) {
		return
	}
	if err := h.in.Channel(observer, "mqtt", store.ChannelStats{T: tRx, AirtimeShort: m.AirtimeShort,
		AirtimeLong: m.AirtimeLong, UtilShort: m.UtilShort, UtilLong: m.UtilLong, RSSI: m.RSSI,
		NoiseFloor: m.NoiseFloor, Interference: m.Interference}); err != nil {
		h.log.Error("ingest failed", "observer", observer, "err", err)
	}
}

func (h *mqttIngest) Status(network, observer string, m *mqtt.Status, tRx time.Time) {
	if h.wrongNetwork(network, observer) {
		return
	}
	if err := h.st.ObserverStatus(observer, "mqtt", tRx, m.Online, m.Lat, m.Lon); err != nil {
		h.log.Error("observer status failed", "observer", observer, "err", err)
	}
	h.log.Info("observer status", "observer", observer, "online", m.Online, "fw", m.Firmware)
}
