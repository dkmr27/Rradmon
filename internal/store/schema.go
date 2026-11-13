package store

// Times are Unix milliseconds. Hashes are raw bytes: packet hashes and
// destination/identity hashes are 16 bytes (Reticulum's truncated length).
// See docs/spec-v0.md §7.
var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE networks (
	id      INTEGER PRIMARY KEY,
	slug    TEXT NOT NULL UNIQUE,
	freq_hz INTEGER NOT NULL,
	bw_hz   INTEGER NOT NULL,
	sf      INTEGER NOT NULL,
	cr      INTEGER NOT NULL
);

CREATE TABLE observers (
	id         INTEGER PRIMARY KEY,
	ext_id     TEXT NOT NULL UNIQUE,
	name       TEXT,
	source     TEXT,
	lat        REAL,
	lon        REAL,
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL
);

CREATE TABLE packets (
	id         INTEGER PRIMARY KEY,
	network_id INTEGER NOT NULL REFERENCES networks(id),
	hash       BLOB NOT NULL,
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL,
	pkt_type   INTEGER NOT NULL,
	dest_type  INTEGER NOT NULL,
	context    INTEGER NOT NULL,
	dest_hash  BLOB NOT NULL,
	data_len   INTEGER NOT NULL,
	ifac       INTEGER NOT NULL DEFAULT 0,
	rx_count   INTEGER NOT NULL DEFAULT 0,
	payload    BLOB,
	h1         BLOB,
	h2         BLOB,
	repeat_of  INTEGER REFERENCES packets(id) ON DELETE SET NULL,
	tx_identity BLOB
);
CREATE INDEX packets_hash ON packets(network_id, hash, first_seen);
CREATE INDEX packets_dest ON packets(dest_hash, first_seen);
CREATE INDEX packets_first_seen ON packets(first_seen);
CREATE INDEX packets_h1 ON packets(h1) WHERE h1 IS NOT NULL;
CREATE INDEX packets_h2 ON packets(h2) WHERE h2 IS NOT NULL;

CREATE TABLE receptions (
	id           INTEGER PRIMARY KEY,
	packet_id    INTEGER NOT NULL REFERENCES packets(id) ON DELETE CASCADE,
	observer_id  INTEGER NOT NULL REFERENCES observers(id),
	t_rx         INTEGER NOT NULL,
	t_obs        INTEGER,
	rssi         REAL,
	snr          REAL,
	hops         INTEGER,          -- NULL for an orphaned second half
	header2      INTEGER,
	transport    INTEGER,
	transport_id BLOB,
	frames       INTEGER NOT NULL,
	partial      INTEGER NOT NULL DEFAULT 0,
	delay_ms     INTEGER NOT NULL,
	tx_identity  BLOB
);
CREATE INDEX receptions_packet ON receptions(packet_id);
CREATE INDEX receptions_observer ON receptions(observer_id, t_rx);
CREATE INDEX receptions_t ON receptions(t_rx);
CREATE INDEX receptions_tx ON receptions(tx_identity, t_rx) WHERE tx_identity IS NOT NULL;

CREATE TABLE destinations (
	hash           BLOB PRIMARY KEY,
	identity_hash  BLOB,
	name_hash      BLOB,
	app_name       TEXT,
	display_name   TEXT,
	first_seen     INTEGER NOT NULL,
	last_seen      INTEGER NOT NULL,
	announce_count INTEGER NOT NULL DEFAULT 0,
	last_hops      INTEGER,
	lat            REAL,
	lon            REAL,
	pos_source     TEXT
);
CREATE INDEX destinations_identity ON destinations(identity_hash);

CREATE TABLE announces (
	id            INTEGER PRIMARY KEY,
	packet_id     INTEGER NOT NULL UNIQUE REFERENCES packets(id) ON DELETE CASCADE,
	dest_hash     BLOB NOT NULL,
	identity_hash BLOB NOT NULL,
	public_key    BLOB NOT NULL,
	name_hash     BLOB NOT NULL,
	random_hash   BLOB NOT NULL,
	ratchet       BLOB,
	app_data      BLOB,
	t_rx          INTEGER NOT NULL,
	hops          INTEGER NOT NULL,
	sig_valid     INTEGER NOT NULL,
	t_announced   INTEGER
);
CREATE INDEX announces_dest ON announces(dest_hash, t_rx);
CREATE INDEX announces_t ON announces(t_rx);

CREATE TABLE channel_stats (
	observer_id  INTEGER NOT NULL REFERENCES observers(id),
	t            INTEGER NOT NULL,
	airtime_s    REAL,
	airtime_l    REAL,
	util_s       REAL,
	util_l       REAL,
	rssi         REAL,
	noise_floor  REAL,
	interference REAL
);
CREATE INDEX channel_stats_obs ON channel_stats(observer_id, t);

-- Orphaned halves of split packets, short frames and unparseable packets.
-- An orphan later matched to a whole packet becomes a partial reception and
-- keeps matched_packet_id here.
CREATE TABLE frame_errors (
	id          INTEGER PRIMARY KEY,
	observer_id INTEGER NOT NULL REFERENCES observers(id),
	t           INTEGER NOT NULL,
	kind        TEXT NOT NULL,
	rssi        REAL,
	snr         REAL,
	len         INTEGER NOT NULL,
	header      INTEGER,
	frame_hash  BLOB,
	data        BLOB,
	matched_packet_id INTEGER REFERENCES packets(id) ON DELETE SET NULL
);
CREATE INDEX frame_errors_obs ON frame_errors(observer_id, t);
CREATE INDEX frame_errors_hash ON frame_errors(frame_hash) WHERE frame_hash IS NOT NULL;

CREATE TABLE node_stats_hourly (
	tx_identity BLOB NOT NULL,
	observer_id INTEGER NOT NULL,
	hour        INTEGER NOT NULL,
	rx_count    INTEGER NOT NULL,
	rssi_sum    REAL NOT NULL,
	rssi_min    REAL NOT NULL,
	rssi_max    REAL NOT NULL,
	snr_sum     REAL NOT NULL,
	snr_min     REAL NOT NULL,
	snr_max     REAL NOT NULL,
	announces   INTEGER NOT NULL,
	PRIMARY KEY (tx_identity, observer_id, hour)
);

CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`,
	// 2: observer status as reported (MQTT status message, or the local RNode)
	`
ALTER TABLE observers ADD COLUMN online      INTEGER;
ALTER TABLE observers ADD COLUMN firmware    TEXT;
ALTER TABLE observers ADD COLUMN software    TEXT;
ALTER TABLE observers ADD COLUMN status_t    INTEGER;  -- when the last status arrived
ALTER TABLE observers ADD COLUMN status_json TEXT;     -- the last status message, as received
`,
}
