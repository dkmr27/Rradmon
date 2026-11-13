# Rradmon Phase 0 spec (draft)

Status: agreed draft, 2026-11-12. Covers the wire formats Rradmon reads, packet
identity, the MQTT observer message v1, the internal event, and the SQLite model.
Decisions are listed in §8.

Sources read: Rrptr `src/lora/LoRaInterface.{h,cpp}`, `src/settings/Settings.cpp`;
microReticulum_Firmware (RNode fork) `RNode_Firmware.ino`, `Framing.h`,
`Utilities.h`, `Config.h`, `sx126x.cpp`. Upstream markqvist/RNode_Firmware should
be checked for drift before Phase 1 (see "To verify").

---

## 1. On-air LoRa frame (RNode format)

Settled from Rrptr and the RNode firmware; both agree.

| Item | Value |
| --- | --- |
| LoRa header | Explicit, CRC on |
| Sync word | `0x12` (private). SX127x register `0x12`; SX126x/LR11x0 `0x1424` (RNode also accepts `0x4434` on read-back) |
| Byte 0 | RNode header: high nibble = random sequence (`& 0xF0`), bit 0 = split flag (`& 0x01`) |
| Bytes 1.. | Reticulum packet bytes |
| Single frame | Reticulum packet ≤ 254 bytes |
| Split | Packet 255..508 bytes: two frames, same header byte, 254 bytes then the rest, sent back to back |

Reassembly (server side, per observer):

1. Split flag clear → whole packet, drop any waiting half (count an orphan).
2. Split flag set, waiting half has the same sequence nibble, combined ≤ 508 → join.
   RSSI/SNR of the joined packet = mean of the two frames (as RNode and Rrptr do);
   keep both per-frame values too.
3. Otherwise → store as a new waiting half (count an orphan if one was waiting).

A half that waits longer than ~2× the frame airtime is an orphan. Orphans and
frames shorter than 2 bytes are health signals: count them, do not drop silently.

Note: the sequence is only 4 bits of randomness, so two observers' reassembly can
disagree under collisions. Reassembly is per observer, before deduplication.

### Partial receptions (heard one half of a split packet)

Goal: when one transmission is heard whole somewhere and only half somewhere
else (another observer, or the same observer hearing the original and a relay),
record the half as a partial reception of that packet. Long packets failing
on marginal links is a health signal that is otherwise invisible.

Which half an orphan is:

- First half: split flag set, payload exactly 254 bytes, and its first bytes
  parse as a Reticulum header. Header fields (type, hops, destination) are
  readable even without the second half.
- Second half: split flag set, payload < 254 bytes. Opaque on its own.
- A 254-byte frame that does not parse as a header is ambiguous: keep it as
  "unknown half".

Matching: for every reassembled split packet, store a hash of each frame's
payload (`h1 = SHA-256(first 254 bytes)[:16]`, `h2 = SHA-256(rest)[:16]`).
An orphan's payload hash is looked up against `h1`/`h2` of packets seen within
the dedupe window. A match becomes a `reception` with `partial = 1` or `2`
(and the orphan's own RSSI/SNR); no match stays in `frame_errors` as an orphan,
with its parsed header if it is a first half. Orphans heard before the whole
packet arrives are re-checked when it does (keep unmatched orphans ~1 min in
memory, then persist them).

Relays change hops (and H1 → H2 adds 16 bytes), so a relayed copy splits at a
different offset and its halves will not match the original's halves. Matching
works per transmission: an orphan matches the whole copy with the same bytes.
That is enough for "observer B heard half of what observer A heard whole".

Status: built in Phase 1.

### Partly heard transmissions (built 2026-11-13)

When no observer hears a split packet whole, its halves cannot become
partial receptions. Instead:

- **Orphan groups:** unmatched orphans with the same frame hash within 10 s
  are one transmission heard by several observers; they share `group_id`
  (the first orphan's id). The feed shows one row per group, listing the
  observers, and a first half's header (type, destination, hops).
- **Proofs of unheard packets:** a PROOF names the packet it proves
  (`Packet.ProvedHash`: the destination for SINGLE proofs and link-request
  proofs; the first 16 bytes of the data for link packet proofs, which RNS
  sends unencrypted as packet hash + signature; none for resource proofs).
  If no packet with that hash was heard, the proof gets `proves_unheard`.
- **Likely link:** such a proof is linked to the latest unlinked orphan group
  in the 15 s before it (`packets.proves_orphan`, `frame_errors.proof_packet_id`),
  in either arrival order. This is a heuristic, shown as "likely" in the UI.
  When the group has a first half, its destination's identity attributes the
  proof's transmitter.

Seen on the bench: a split probe from rnsd reached both observers only as its
first half, yet Rrptr proved it 0.7 s later; another reached both only as its
second half and was never answered (rnprobe timed out).

## 2. RNode host protocol in promiscuous mode

KISS framing: `FEND 0xC0`, `FESC 0xDB`, `TFEND 0xDC`, `TFESC 0xDD`.

Enable with `CMD_PROMISC 0x0E` value `0x01`. In promiscuous mode the firmware
skips reassembly and sends **every raw LoRa frame including the RNode header
byte**. Each received frame arrives as three KISS frames, in this order:

| Cmd | Byte | Payload | Decode |
| --- | --- | --- | --- |
| `CMD_STAT_RSSI` | `0x23` | 1 byte | `rssi_dbm = byte - 157` |
| `CMD_STAT_SNR` | `0x24` | 1 byte, signed | `snr_db = int8(byte) * 0.25` |
| `CMD_DATA` | `0x00` | raw frame | header byte + payload |

Verified 2026-11-12 on a T3S3 RNode (ESP32-S3, firmware 1.86) on COM32: 12/12
probes and 12/12 proofs captured, header byte present, split bit clear.

Caveat: in the microReticulum_Firmware fork source, the ESP32/nRF52 promiscuous
branch sets `packet_ready` but never queues the frame, so that build would
deliver nothing. The COM32 firmware does not have this problem. Rradmon keeps a
normal (non-promiscuous) mode as a fallback: the RNode then sends whole
Reticulum packets without the header byte (verified on COM32: 8/8 probe
packets). Normal mode loses split-frame and orphan visibility.

Test rig: rnsd on this PC with an RNode on COM19 probing a Rrptr on COM15
(`rnprobe rnstransport.probe 14db86ef43369aca88ac657156c9fc90`) gives known
DATA (132 B raw) and PROOF (84 B raw) frames on demand.

Useful extra reports (ESP32/nRF52 firmware):

| Cmd | Byte | Payload | Use |
| --- | --- | --- | --- |
| `CMD_STAT_CHTM` | `0x25` | `ats u16, atl u16, cls u16, cll u16, crs u8, nfl u8, ntf u8` | Airtime short/long term and channel utilisation short/long term, each `% × 100`; current RSSI, noise floor, interference RSSI (`-157` offset; `ntf 0xFF` = none). Sent periodically. |
| `CMD_STAT_PHYPRM` | `0x26` | six u16 | Symbol time, symbol rate, preamble symbols/time, CSMA slot, DIFS. Sent on config change. |

Channel utilisation and noise floor are the best "mesh health" numbers we get
for free; Phase 1 should store them (see `channel_stats`).

Config commands needed at startup: `CMD_FREQUENCY 0x01` (u32 Hz),
`CMD_BANDWIDTH 0x02` (u32 Hz), `CMD_SF 0x04`, `CMD_CR 0x05`, `CMD_TXPOWER 0x03`,
`CMD_RADIO_STATE 0x06` (`0x01` on), `CMD_DETECT 0x08`, `CMD_FW_VERSION 0x50`.
Rradmon never transmits; set TX power to the minimum.

## 3. Reticulum packet parsing

```
byte 0   flags: b7 IFAC | b6 header type (0=H1, 1=H2) | b5 context flag
               | b4 propagation (0 broadcast, 1 transport)
               | b3-2 destination type (single, group, plain, link)
               | b1-0 packet type (data, announce, link request, proof)
byte 1   hops
[IFAC]   if b7: IFAC bytes, packet masked -> record flags/len only, stop
H1:      dest hash (16)                     then context (1), data
H2:      transport id (16), dest hash (16)  then context (1), data
```

Per reception we extract: packet type, destination type, propagation, header
type, context flag, hops, transport id (H2), destination hash, context byte,
data length.

Announce data (not encrypted): public key (64) · name hash (10) · random hash
(10) · ratchet (32, only if context flag) · signature (64) · app data.
Identity hash = `SHA-256(public key)[:16]`. App data often carries a display name
(msgpack for LXMF; parse best-effort).

Announce validation (v1, as RNS `Identity.validate_announce`):

1. Public key = X25519 (first 32 bytes) || Ed25519 (last 32 bytes).
2. `signed = dest_hash || public_key || name_hash || random_hash || ratchet || app_data`
   (ratchet empty when the context flag is clear).
3. `ed25519.Verify(pub[32:], signed, signature)` — Go stdlib `crypto/ed25519`.
4. `dest_hash == SHA-256(name_hash || identity_hash)[:16]`.

Store the result as `sig_valid` (1 valid, 0 bad signature, 2 hash mismatch).
Invalid announces are kept and shown flagged, never used to name or place a node.

The random hash is 5 random bytes + a 5-byte big-endian Unix timestamp (seconds)
of when the announce was made. That gives announce age at reception, which
measures how long relays queued it. It depends on the announcing node's clock,
so treat it as an estimate and ignore values in the future or > 1 day old.
microReticulum nodes without a real-time clock (e.g. Rrptr) put a node-relative
seconds counter there instead (seen: `0x0000001238`); Rradmon treats any value
outside a plausible Unix-time range as unknown. Relay queueing delay is still
measurable as the gap between the original (hops 0) and the relayed copy: on
the bench rig rnsd rebroadcast three announces 1.5 s, 21 s and 41 s after the
originals, which is its announce cap at work.

Interface discovery announces (`rnstransport.discovery.interface`, a SINGLE
destination) carry app data `flags (1) || msgpack map || stamp (32)`; flag
0x02 means the map is encrypted to a network identity (then unreadable). Map
keys (RNS `Discovery.py`): 0x00 interface type, 0x01 transport, 0x02
reachable on, 0x03 latitude, 0x04 longitude, 0x05 height, 0x06 port,
0x07 IFAC netname, 0x09 frequency, 0x0A bandwidth, 0x0B SF, 0x0C CR,
0x0D modulation, 0x0E channel, 0xFC transport version, 0xFD implementation,
0xFE transport ID, 0xFF name. Verified 2026-11-12 on Rrptr 0.1.0 (150 B app
data, unencrypted, sent as a split packet). This is Phase 3's position
source; positions are real locations, so keep such captures out of the repo.

Built 2026-11-13 (`reticulum.DescribeAnnounce`, a bounded msgpack decoder):
every announce's app data is summarised in one line for the live view and
the node page. Discovery: name, interface, implementation and version, radio
settings, reachable-on and IFAC network name when present, and "position"
with height (never the coordinates in the summary). LXMF delivery: display
name and stamp cost. NomadNet and other printable app data: the text.
A valid discovery announce sets its destination's lat, lon, height and
`pos_source = 'discovery'`; the newest announce wins. The map shows each node
at its newest announced position, and observers at the position they report
(`rradmon run -lat -lon` for a local RNode, `observe -lat -lon`, Rrobs
`RROBS_LAT/LON`). Map tiles come from OpenStreetMap and can be turned off.
Manual pins need a write API and so wait for admin login (Phase 4).

Verified on the bench corpus (`testdata/rig-corpus1.jsonl`): all 7 proofs
match the truncated hash of an earlier packet (probes, split probes and link
requests), link traffic is addressed to the link request's truncated hash, all
6 announces validate, and relayed announces keep the original's packet hash.

## 4. Packet identity (dedupe) — proposed

Use Reticulum's own packet hash:

```
hashable = (flags & 0x0F) || raw[2:]           # H1
hashable = (flags & 0x0F) || raw[18:]          # H2, skips transport id
packet_hash = SHA-256(hashable)                # store 32 bytes, index on first 16
```

This drops hops, the header type and the transport id, so the **same packet
relayed by a transport node keeps the same hash**. That is what we want:

- Two observers hearing the same transmission → one `packet`, two `reception`s.
- One observer hearing the original (hops 0) and a relay (hops 1, transport id X)
  → one `packet`, two `reception`s with different hops/transport id. This is the
  raw material for topology: X relays traffic from the originator.

Dedupe window: how long after `first_seen` a reception still joins the same
`packet`. Two values, both settings:

| Packet type | Window | Why |
| --- | --- | --- |
| Announce | 4 h | RNS relays queue announces on capped interfaces for up to 3 h (`QUEUED_ANNOUNCE_LIFE`, 2% `ANNOUNCE_CAP`, lowest hops first). A 10 min window would split one announce into several packets on a busy LoRa channel. |
| Everything else | 10 min | Data, link and proof packets carry fresh encryption or random IVs, so identical bytes only come from forwarding or link-layer retransmits seconds apart. |

Each reception stores `delay_ms = t_rx - packet.first_seen`. For announces we also
know when the announce was made (random hash timestamp), so relay queueing delay
is measured directly.

To find out whether the windows are wrong in practice: when a hash arrives after
its window closed and an older `packet` with that hash exists (look back 24 h),
the new packet records `repeat_of`. A dashboard counter of repeats, and of
delays near the window edge, shows if the windows need widening.

IFAC packets: hash the whole masked frame instead, flagged `ifac=1`.

## 5. MQTT observer message v1

Topic layout (observer id in the path so broker ACLs can pin publishers):

```
rradmon/v1/<network_id>/<observer_id>/rx        one message per LoRa frame
rradmon/v1/<network_id>/<observer_id>/chan      channel stats (optional)
rradmon/v1/<network_id>/<observer_id>/status    retained; also the last-will
```

`network_id` is a short slug for the radio profile, e.g. `au915-925875-250-9-5`.

Observers publish **raw frames**, not reassembled packets. The server owns
reassembly so every observer behaves the same.

`rx` payload (JSON, binary as base64):

```json
{
  "v": 1,
  "observer": "obs-7f3a2c",
  "seq": 18233,
  "t_obs": "2026-11-12T03:14:15.926Z",
  "radio": {"freq_hz": 925875000, "bw_hz": 250000, "sf": 9, "cr": 5, "sync": 18},
  "rssi": -97.0,
  "snr": 6.25,
  "len": 143,
  "frame": "<base64 of raw frame incl. RNode header byte>",
  "stripped": false,
  "frame_hash": "<hex, SHA-256 of the full frame, set when stripped>",
  "observer_key": null,
  "sig": null
}
```

- `seq` is a per-observer counter; gaps reveal lost MQTT messages.
- `t_obs` is the observer's claim; the server adds `t_rx` and orders by it.
- `stripped=true` means `frame` holds only the RNode header + Reticulum header
  (and full announce data); `len` is the original length and `frame_hash` is
  the SHA-256 of the full frame. Rradmon's own observers never strip (§8);
  the field exists for republishing. A stripped split frame cannot be
  reassembled, so a stripped message must carry a whole packet's hash instead
  (defined when republishing is built).
- `observer_key`/`sig` are reserved; v1 servers ignore them.

`status` payload: `{"v":1,"observer":"…","online":true,"fw":"…","radio":{…},"lat":…,"lon":…,"uptime_s":…}`;
last-will publishes `{"v":1,"observer":"…","online":false}` retained.

`chan` payload: the `CMD_STAT_CHTM` fields decoded (percentages and dBm).

Validation on ingest: `v` known, `len` ≤ 255 (one frame), `rssi` in −160..0,
`snr` in −32..32, `t_obs` within ±1 day of `t_rx`, rate limit per observer.

As implemented (`internal/mqtt`, 2026-11-12):

- `frame` is base64 (Go `[]byte` JSON). Optional `"raw": false` marks a whole
  Reticulum packet from an RNode in normal mode (then up to 508 bytes);
  absent means a raw frame with the RNode header byte (2–255 bytes).
- Observers never send `stripped` frames; the server rejects them.
- The topic's network segment must equal the message's radio
  (`<freq>-<bw>-<sf>-<cr>`), and the topic's observer must equal `observer`
  (`[A-Za-z0-9._-]{1,64}`). One radio network per server for now; others
  are logged and ignored.
- Limits: 4 KB per message, 20 messages/s per observer with a burst of 100,
  optional allowlist (`run -mqtt-allow`). `seq` gaps are counted per
  observer; a seq of 1 is treated as an observer restart.
- QoS 1 for `rx` and `status` (retained, also the last will), QoS 0 for `chan`.
- Status carries `sw` (software) and `fw` (radio firmware) as well.
- Credentials: username flag, password from a file (`-mqtt-pass-file`) or
  `RRADMON_MQTT_PASSWORD`, never a flag or the URL. TLS (`ssl://`, `wss://`)
  is verified. Bench broker: password in `secret/mqtt_password`.

Verified on the bench: `rradmon observe -port COM32` → mosquitto →
`rradmon run -mqtt …` stored announces, probes, link traffic and split
packets as with a local RNode; two replay observers publishing the same
capture at once produced 27 packets with 2 receptions each and no orphans.

## 6. Internal event (Go)

Both sources emit the same value into the ingestor:

```go
type FrameEvent struct {
    ObserverID string
    Source     string        // "rnode-serial" | "mqtt:<broker>"
    TObs       time.Time     // zero if unknown
    TRx        time.Time     // set by Rradmon
    Radio      RadioConfig
    RSSI       float32
    SNR        float32
    Frame      []byte        // raw, incl. RNode header byte
    Stripped   bool
    OrigLen    int
}

type ChannelEvent struct { ObserverID string; TRx time.Time; AirtimeShort, AirtimeLong, UtilShort, UtilLong float32; RSSI, NoiseFloor float32; Interference *float32 }
type ObserverStatusEvent struct { ObserverID string; TRx time.Time; Online bool; Firmware string; Lat, Lon *float64 }
```

Pipeline: `FrameEvent` → per-observer reassembler → `PacketEvent` (Reticulum
bytes + per-frame signal) → parser → dedupe → store → live feed.

## 7. SQLite model

```sql
networks   (id INTEGER PK, slug TEXT UNIQUE, freq_hz, bw_hz, sf, cr, sync)
observers  (id INTEGER PK, ext_id TEXT UNIQUE, name, source, lat, lon,
            first_seen, last_seen, online, trust TEXT)        -- trust: local|trusted|untrusted
packets    (id INTEGER PK, network_id, hash BLOB(32), first_seen, last_seen,
            pkt_type, dest_type, context, dest_hash BLOB(16), data_len,
            ifac INTEGER, rx_count)                             -- index (network_id, hash, first_seen)
            -- + payload BLOB NULL (Reticulum packet bytes; see payload storage)
            -- + h1 BLOB(16), h2 BLOB(16) (frame payload hashes, split packets only)
            -- + repeat_of INTEGER NULL (older packet with the same hash, outside the window)
receptions (id INTEGER PK, packet_id, observer_id, t_rx, t_obs, rssi, snr,
            hops, header_type, propagation, transport_id BLOB(16),
            frames INTEGER, partial INTEGER, delay_ms INTEGER)  -- partial: 0 whole, 1/2 half heard
                                                                -- index (observer_id, t_rx), (packet_id)
destinations (hash BLOB(16) PK, identity_hash BLOB(16), name_hash BLOB(10),
            display_name, first_seen, last_seen, announce_count, last_hops,
            lat, lon, pos_source)                               -- pos_source: discovery|manual|null
announces  (id INTEGER PK, packet_id, dest_hash, identity_hash, public_key BLOB(64),
            name_hash, random_hash, ratchet BLOB, app_data BLOB, t_rx, hops,
            sig_valid INTEGER, t_announced)                     -- t_announced from random hash
node_stats_hourly (dest_hash, observer_id, hour, rx_count, rssi_min, rssi_avg,
            rssi_max, snr_min, snr_avg, snr_max, announces)     -- PK (dest_hash, observer_id, hour)
channel_stats (observer_id, t_rx, airtime_s, airtime_l, util_s, util_l,
            rssi, noise_floor, interference)                    -- index (observer_id, t_rx)
frame_errors (observer_id, t_rx, kind, rssi, snr, len)          -- orphan|short|parse|ifac
settings   (key TEXT PK, value TEXT)                            -- retention days etc.
```

WAL mode on.

Payload storage: setting `ingest.store_payloads`, default **on** while we build
(handy for debugging the parser and building the test corpus). When off, the
ingestor sets `packets.payload` to NULL for everything except announces, after
the packet hash and frame hashes are computed. Announces are always kept whole.
The same setting applies to anything Rradmon later republishes over MQTT
(`stripped=true`). Turning it off does not rewrite old rows; a one-off
"strip stored payloads" admin action does that.

`node_stats_hourly` is filled as receptions arrive (upsert per hour bucket),
so it does not depend on raw rows still existing.

Retention: `settings` holds days per class, purged by an hourly job that
deletes in batches, then removes `packets` with no remaining receptions.

| Setting | Default |
| --- | --- |
| `retain.receptions_days` (receptions, packets, frame_errors) | 14 |
| `retain.announces_days` | 60 |
| `retain.channel_days` | 30 |
| `retain.node_stats_days` | 30 |

Destinations are kept while any announce or reception refers to them, or for
`retain.announces_days` after `last_seen`.

## 8. Decisions (agreed 2026-11-12)

- Framing/splitting §1, sync word `0x12`, packet identity §4.
- Observers publish raw frames; the server reassembles.
- Payloads stored during development; dropping them is an ingestor setting (§7).
  Observers always send full frames, so dedupe and frame hashes never see a
  stripped packet. `stripped` stays in the MQTT schema for later republishing.
- Retention: receptions 14 d, announces 60 d, channel stats 30 d, hourly
  per-node aggregates 30 d (§7).
- Dedupe window: 10 min, except announces 4 h; delays and late repeats are
  measured so we can see if this is wrong (§4).
- Announce signatures and destination hashes validated in v1 (§3).
- Partial receptions: designed in, built as a Phase 1 stretch goal (§1).
- Order: RNode on the dedicated test port (COM32) first. Observer second: a
  receive-only Heltec V3 (SX1262) build reusing Rrptr's `src/radio` HAL,
  publishing raw frames over MQTT.

## 9. To verify

- Upstream RNode_Firmware still emits RSSI/SNR/DATA in this order in promiscuous
  mode, and the `CMD_STAT_CHTM` layout, for current releases.
- Whether promiscuous mode still applies the radio CRC check (frames with CRC
  errors are likely dropped by the chip, so CRC errors are invisible to us).
- Capture a test corpus from both radios: single frames, split packets,
  announces with/without ratchets, H2 relayed packets.
