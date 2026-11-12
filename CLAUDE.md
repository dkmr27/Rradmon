# Rradmon

Reticulum LoRa radio monitor. Single Go binary, web UI embedded, SQLite store, MIT.

- Plan (living doc): https://claude.ai/artifact/E5zdizsXet4RmtobKfivDA
- Wire formats, MQTT schema, data model: `docs/spec-v0.md` (source of truth for code)

## Layout

- `cmd/rradmon` — entry point
- `internal/` — packages (sources, reassembly, parser, store, web) as they are added

## Reference code on this machine

- Rrptr (LoRa Reticulum transport node, MIT): `C:\Users\vk2tr\Documents\AI\Claude\Rrptr`
  — `src/lora/LoRaInterface.*` for RNode on-air framing, `src/radio/` HAL
- RNode firmware fork: `C:\Users\vk2tr\Documents\AI\Claude\microReticulum_Firmware`
  — `Framing.h` KISS commands, `RNode_Firmware.ino` promiscuous receive path

## Commands

- `go test ./...` — unit tests plus bench-corpus tests in `testdata/`
- `rradmon run -port COM32 -db bench.db [-for 3m]` — live ingest into SQLite
- `rradmon capture -port COM32 [-for 5m]` — raw frames to `captures/` (git-ignored)
- `rradmon decode|import <capture.jsonl>` — replay a capture
- `rradmon observe -port COM32 -id obs-bench-t3s3 $M` — RNode to MQTT (or `-replay file` to publish a capture)
- `rradmon run $M -db x.db` — ingest from MQTT (add `-port` to also read a local RNode)
- Bench broker flags: `source secret/mqtt.env` sets `$M`. Broker address, username and password
  live only in `secret/` (git-ignored); never put them in a flag value, URL, tracked file, commit or log.

## Bench rig (generating known traffic)

- rnsd on this PC (shared instance, RNode on COM19) ↔ Rrptr on COM15; sniffer RNode (T3S3, 20 dB attenuated) on COM32.
- Probe: `rnprobe rnstransport.probe 14db86ef43369aca88ac657156c9fc90` (`-s 300` forces a split packet).
  Run via `python -m RNS.Utilities.rnprobe` with `PYTHONUNBUFFERED=1 PYTHONIOENCODING=utf-8` or the reply line is lost.
- Links: `rnstatus -R 135a9914ef8e82c36ea3c82bd4689e02 -i secret/test_identity` (secret/ is git-ignored).
- Announces: send `an\r\n` to COM15 at 115200 (Rrptr CLI).

## Rules

- Never store encrypted payloads; metadata only (spec §7).
- Reassembly happens per observer, before dedupe (spec §1, §4).
- Rradmon never transmits on the radio.
- Test RNode: COM32, dedicated to Rradmon testing. Observer hardware: Heltec V3 (SX1262).
- Repo is private (github.com/dkmr27/Rradmon); keep it that way until the user says otherwise.
- Network: AU915, 925.875 MHz, 250 kHz, SF9, CR 4/5, sync 0x12. Store radio config with every observation.
