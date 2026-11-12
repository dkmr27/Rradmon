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

## Rules

- Never store encrypted payloads; metadata only (spec §7).
- Reassembly happens per observer, before dedupe (spec §1, §4).
- Rradmon never transmits on the radio.
- Test RNode: COM32, dedicated to Rradmon testing. Observer hardware: Heltec V3 (SX1262).
- Repo is private (github.com/dkmr27/Rradmon); keep it that way until the user says otherwise.
- Network: AU915, 925.875 MHz, 250 kHz, SF9, CR 4/5, sync 0x12. Store radio config with every observation.
