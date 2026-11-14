# Rradmon

A monitor for Reticulum LoRa networks. Plug in an RNode and Rradmon shows what
the network around you is doing: every packet heard with its RSSI and SNR, the
nodes and their announces, channel noise and utilisation, and a map of nodes
whose position is known. Everything is stored in a local SQLite database, so
there is history as well as a live view.

More receivers can join over MQTT: ESP32 observers or other PCs with an RNode
each report what they hear, and Rradmon combines them, so you can see which
receiver heard which node and how well.

Rradmon only listens. It never transmits on the radio.

Status: early (0.2.0). It runs well on the author's bench; expect rough edges.

## What you see

- **Live**: one row per packet, expanding to every reception of it (which
  observer, RSSI, SNR, hops, relay). Packet type, destination, announce
  details and proofs are decoded where Reticulum leaves them readable.
- **Nodes**: every destination heard, searchable by name or hash, with announce
  rates and best signal. A node page has signal history per observer and
  announce history.
- **Observers**: each receiver with its noise floor and channel utilisation
  over time, and the nodes it hears directly.
- **Map**: nodes that publish a position (Reticulum interface discovery), and
  nodes or observers you place by hand. Click an observer to draw lines to the
  nodes it heard, coloured by average SNR.

Most Reticulum traffic is encrypted and data packets carry no sender address,
so Rradmon shows what is visible on air and makes the best of it.

## Requirements

- An RNode with firmware that supports promiscuous mode. Tested with RNode
  firmware 1.86 on a LilyGO T3S3. If your firmware's promiscuous mode delivers
  nothing, use `-promisc=false`: you then see whole packets only, without
  split-frame detail.
- Windows, macOS or Linux, including a Raspberry Pi. Nothing else to install.
  A Docker image is also available for servers (see Docker).
- Optional: an MQTT broker (such as Mosquitto) if you want more than one
  receiver.

## Install

Download the archive for your platform from the
[releases](https://github.com/dkmr27/Rradmon/releases), unpack it, and run
`rradmon` from a terminal. Each archive holds one program file.

| Platform | Archive |
|---|---|
| Windows (most PCs) | `rradmon-<version>-windows-amd64.zip` |
| Windows on ARM | `rradmon-<version>-windows-arm64.zip` |
| Linux PC | `rradmon-<version>-linux-amd64.tar.gz` |
| Raspberry Pi 3, 4, 5 with 64-bit OS | `rradmon-<version>-linux-arm64.tar.gz` |
| Any Raspberry Pi with 32-bit OS (Zero included) | `rradmon-<version>-linux-armv6.tar.gz` |
| Mac with Apple silicon | `rradmon-<version>-macos-arm64.tar.gz` |
| Intel Mac | `rradmon-<version>-macos-amd64.tar.gz` |

Check a download against `rradmon-<version>-SHA256SUMS` if you like:
`sha256sum -c` on Linux, `shasum -a 256 -c` on macOS, or `Get-FileHash` in
PowerShell.

Platform notes:

- **Windows**: the program is not signed, so SmartScreen may warn on first
  run (More info, then Run anyway). The RNode needs its USB serial driver
  (CP210x or CH340, depending on the board); it then appears as a COM port.
- **macOS**: the program is not signed, so macOS blocks it the first time.
  Either right-click it in Finder and choose Open, or run
  `xattr -d com.apple.quarantine rradmon` once.
- **Linux**: your user needs access to the serial port. On Debian, Ubuntu
  and Raspberry Pi OS: `sudo usermod -aG dialout $USER`, then log out and in.

## Quick start

Find the RNode's serial port:

```
rradmon ports
```

It is a `COM` port on Windows, `/dev/ttyUSB0` or `/dev/ttyACM0` on Linux, and
`/dev/cu.usbserial-…` or `/dev/cu.usbmodem…` on macOS. Then start Rradmon
with your network's radio settings:

```
rradmon run -port COM5 -freq 925875000 -bw 250000 -sf 9 -cr 5
```

Open http://localhost:8080 in a browser. Packets appear as they are heard.
The database is `rradmon.db` in the current folder (`-db` to choose another).
Press Ctrl+C to stop; run the same command again to carry on with the same
database.

The radio settings must match the network you want to hear. The defaults are
925.875 MHz, 250 kHz, SF9, CR 4/5 (an AU915 network), so set yours with
`-freq`, `-bw`, `-sf` and `-cr`.

To place your own receiver on the map, add `-lat` and `-lon`, or set its
position by hand in the UI.

## Several receivers over MQTT

Observers publish every LoRa frame they hear to an MQTT broker; Rradmon
subscribes and combines them. Two kinds of observer exist:

- **Rrobs**, a separate receive-only firmware for the Heltec V3 (ESP32 with
  SX1262), which connects over Wi-Fi.
- **`rradmon observe`** on any PC or Raspberry Pi with an RNode attached.

An observer on a Raspberry Pi:

```
rradmon observe -port /dev/ttyUSB0 -id garden-pi -mqtt tcp://192.168.1.10:1883 -mqtt-user garden-pi -mqtt-pass-file broker.pass
```

The Rradmon server, reading the broker and, if you like, its own RNode too:

```
rradmon run -mqtt tcp://192.168.1.10:1883 -mqtt-user rradmon -mqtt-pass-file broker.pass -port COM5
```

- Give each observer a stable, unique `-id`; it is how Rradmon tells them
  apart.
- Observers must use the same radio settings. Rradmon ignores observers on
  another network and logs a warning.
- Broker URLs can be `tcp://`, `ssl://`, `ws://` or `wss://`. Use `ssl://` or
  `wss://` for a broker beyond your own network; certificates are always
  verified.
- Put the broker password in a file (`-mqtt-pass-file`) or the
  `RRADMON_MQTT_PASSWORD` environment variable, never in the command line,
  where other users of the machine can see it.
- `-mqtt-allow obs-a,obs-b` accepts only the listed observers.
- `-local <observer id>` makes an MQTT observer the "local" one shown on the
  Local tab, for a server that has no RNode of its own.

Topics are `rradmon/v1/<network>/<observer>/rx`, `…/chan` and `…/status`
(`-mqtt-prefix` changes `rradmon/v1`). With the observer id in the path, broker
access rules can limit each observer to its own topics. The message format is
in [docs/spec-v0.md](docs/spec-v0.md), section 5.

## Admin and settings

Anyone who can open the web UI can view it. Changes need the admin password:
the Settings tab, positions set by hand, and removing stored payloads. Set the
password from the command line on the machine that runs Rradmon:

```
rradmon admin -db rradmon.db set-password
```

Then click Admin at the top right of the UI to log in. `clear-password`
turns admin off again.

Settings (Settings tab, admin only):

| Setting | Default |
|---|---|
| Keep receptions | 14 days |
| Keep announces | 60 days |
| Keep channel stats | 30 days |
| Keep hourly node stats | 30 days |
| Keep silent observers (removed once not heard and no receptions left) | 30 days |
| Dedupe window | 10 minutes |
| Dedupe window, announces | 4 hours (relays can hold announces for hours) |
| Store payloads | on (announces are always kept) |
| Local observer | none |

## Docker

A Docker image suits a server that reads MQTT observers, on a home server or
a VPS. It is built from the repository and runs on x86-64, ARM64 and 32-bit
ARM (Raspberry Pi):

```
docker build -t rradmon .
```

(Add `--build-arg VERSION=$(git describe --tags --always)` to stamp the
version.) The image is small (only the program and CA certificates), runs as a
non-root user, and keeps its database in the `/data` volume.

With Docker Compose, using the included [compose.yaml](compose.yaml):

1. Put the broker password in `secret/mqtt_password` and let the container
   read it: `sudo chown 65532 secret/mqtt_password` (65532 is the
   container's user).
2. Create a `.env` file next to `compose.yaml` with the broker address and
   user:
   ```
   RRADMON_MQTT_URL=tcp://192.168.1.10:1883
   RRADMON_MQTT_USER=rradmon
   ```
3. Start it and set the admin password:
   ```
   docker compose up -d
   docker compose exec rradmon rradmon admin set-password
   ```

The web UI is then at http://localhost:8080. To open it to your network, add
`RRADMON_WEB=8080` to `.env` (see Security first). `docker compose logs -f`
shows the log.

To read an RNode on the Docker host as well, create `compose.override.yaml`
next to `compose.yaml` (Compose reads it automatically; it is git-ignored):

```yaml
services:
  rradmon:
    devices: ["/dev/ttyACM0"]
    group_add: ["46"]
    environment:
      RRADMON_SERIAL: /dev/ttyACM0
```

Use your RNode's device. `group_add` is the device's group number, the
fourth column of `ls -ln /dev/ttyACM0` (46 or 20 on a Raspberry Pi);
without it the container's user cannot open the port. If the RNode is
unplugged, Rradmon stops and Docker restarts it; the device has to be
plugged in for the container to start.

Without Compose:

```
docker run -d --name rradmon --restart unless-stopped -p 127.0.0.1:8080:8080 \
  -v rradmon-data:/data -v "$PWD/secret/mqtt_password:/run/secrets/mqtt_password:ro" \
  rradmon run -mqtt tcp://192.168.1.10:1883 -mqtt-user rradmon -mqtt-pass-file /run/secrets/mqtt_password -quiet
```

With an RNode, pass the device and its group, and add `-port`:

```
docker run ... --device /dev/ttyUSB0 --group-add "$(stat -c %g /dev/ttyUSB0)" \
  rradmon run -port /dev/ttyUSB0 ...
```

Notes:

- Inside the container Rradmon listens on all interfaces (`RRADMON_HTTP`
  is set to `0.0.0.0:8080`); the `-p` mapping on the host decides who can
  reach it. `-p 127.0.0.1:8080:8080` keeps it to the host.
- The container has no time zone data, so its log is in UTC. The web UI
  shows times in your browser's time zone.
- To build for other platforms from one machine:
  `docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7,linux/arm/v6 -t rradmon .`

## Security

- The web UI listens on `127.0.0.1:8080`, so only this computer can reach it.
  To open it to your network, use for example `-http 0.0.0.0:8080`; Rradmon
  then warns that it is reachable.
- On a network, set an admin password first. Rradmon serves plain HTTP, so
  the password crosses the network unencrypted. Beyond a home network, put
  Rradmon behind a reverse proxy with HTTPS (Caddy, nginx).
- Admin sessions use an HttpOnly, SameSite=Strict cookie with a CSRF token,
  requests from other sites are refused, and logins are rate limited.
- Data from observers is treated as untrusted: lengths, ranges and text are
  checked and cleaned before they are stored or shown.

## Commands

| Command | What it does |
|---|---|
| `rradmon run` | Read an RNode and/or MQTT observers, store what they hear, serve the web UI |
| `rradmon serve` | Web UI for an existing database, no radio |
| `rradmon observe` | Read an RNode and publish to MQTT as an observer |
| `rradmon admin` | Set or clear the admin password |
| `rradmon ports` | List serial ports |
| `rradmon capture` | Save raw frames from an RNode to a file, for diagnosis |
| `rradmon decode` | Print the packets in a capture file |
| `rradmon import` | Store a capture file in the database |
| `rradmon version` | Print the version |

`rradmon <command> -h` lists a command's options.

## Build from source

With Go 1.23 or later:

```
go build ./cmd/rradmon
go test ./...
```

`go run ./tools/release` builds every platform into `dist/`, with archives
and a checksum file. No C compiler is needed, so any machine can build for
any platform.

## Licence

MIT. See [LICENSE](LICENSE).
