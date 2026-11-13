package store

import (
	"database/sql"
	"time"
)

// Map data: nodes with a known position, and observers that report one.

// MapNode is a node placed on the map.
type MapNode struct {
	NodeRow
	Lat       float64   `json:"lat"`
	Lon       float64   `json:"lon"`
	Height    *float64  `json:"height,omitempty"`
	PosSource string    `json:"pos_source"` // "discovery" (later: "manual")
	PosT      time.Time `json:"pos_t"`      // when that position was announced
	Info      string    `json:"info,omitempty"`
}

// MapObserver is an observer placed on the map.
type MapObserver struct {
	ExtID    string    `json:"ext_id"`
	Source   string    `json:"source"`
	State    string    `json:"state"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	LastSeen time.Time `json:"last_seen"`
}

// MapData is everything the map shows.
type MapData struct {
	Nodes     []MapNode     `json:"nodes"`
	Observers []MapObserver `json:"observers"`
	Unplaced  int           `json:"unplaced"` // nodes with no known position
}

// Map returns placed nodes and observers. A node's position is its most
// recently announced one, from any of its destinations.
func (s *Store) Map(now time.Time) (MapData, error) {
	var md MapData
	rows, err := s.DB.Query(`SELECT d.identity_hash, d.lat, d.lon, d.height, d.pos_source, COALESCE(d.info_t, d.last_seen),
			COALESCE(d.info, '')
		FROM destinations d
		WHERE d.lat IS NOT NULL AND d.lon IS NOT NULL AND d.identity_hash IS NOT NULL
		ORDER BY COALESCE(d.info_t, d.last_seen) DESC`)
	if err != nil {
		return md, err
	}
	type pos struct {
		lat, lon float64
		height   *float64
		source   string
		t        int64
		info     string
	}
	placed := map[string]pos{}
	for rows.Next() {
		var ident []byte
		var p pos
		var src sql.NullString
		if err := rows.Scan(&ident, &p.lat, &p.lon, &p.height, &src, &p.t, &p.info); err != nil {
			rows.Close()
			return md, err
		}
		p.source = src.String
		if _, seen := placed[hx(ident)]; !seen { // newest first
			placed[hx(ident)] = p
		}
	}
	rows.Close()

	nodes, err := s.Nodes("", now)
	if err != nil {
		return md, err
	}
	for _, n := range nodes {
		p, ok := placed[n.Identity]
		if !ok {
			md.Unplaced++
			continue
		}
		md.Nodes = append(md.Nodes, MapNode{NodeRow: n, Lat: p.lat, Lon: p.lon, Height: p.height,
			PosSource: p.source, PosT: fromMs(p.t), Info: p.info})
	}

	obs, err := s.Observers(now)
	if err != nil {
		return md, err
	}
	for _, o := range obs {
		if o.Lat != nil && o.Lon != nil {
			md.Observers = append(md.Observers, MapObserver{ExtID: o.ExtID, Source: o.Source, State: o.State,
				Lat: *o.Lat, Lon: *o.Lon, LastSeen: o.LastSeen})
		}
	}
	return md, nil
}
