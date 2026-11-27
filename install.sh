#!/bin/sh
#
# Install Rradmon as a service on Linux with systemd (Raspberry Pi OS,
# Debian, Ubuntu and others), so it starts at boot and restarts if it stops.
#
#   sudo ./install.sh                  install, or update an existing install
#   sudo ./install.sh --version 0.4.0  a given release instead of the latest
#   sudo ./install.sh --uninstall      remove it (keeps the database and settings)
#
# Run it from an unpacked release archive and it installs the rradmon next
# to it. Run on its own, it downloads the right release for this machine
# from GitHub and checks it against the release's SHA256SUMS.
#
# It installs /usr/local/bin/rradmon, a user rradmon (in the dialout group,
# for the RNode's serial port), settings in /etc/rradmon/rradmon.conf (an
# existing file is kept), the database in /var/lib/rradmon, and the
# rradmon service. A new install asks for the serial port, radio settings,
# who may open the web UI, and the admin password.
#
# For testing: DESTDIR=/some/dir installs the files under that folder, and
# DRY_RUN=1 prints the user and service commands instead of running them.

set -e

# sudo on some systems keeps the caller's PATH, without the sbin folders
PATH=/usr/local/sbin:/usr/sbin:/sbin:$PATH
export PATH

REPO=dkmr27/Rradmon
DESTDIR=${DESTDIR:-}
DRY_RUN=${DRY_RUN:-}
BIN=$DESTDIR/usr/local/bin/rradmon
CONF_DIR=$DESTDIR/etc/rradmon
CONF=$CONF_DIR/rradmon.conf
DATA=/var/lib/rradmon
UNIT=$DESTDIR/etc/systemd/system/rradmon.service
USER_NAME=rradmon
HERE=$(cd "$(dirname "$0")" && pwd)

say() { echo "==> $*"; }
warn() { echo "warning: $*" >&2; }
die() { echo "error: $*" >&2; exit 1; }
# Commands that change the system outside the installed files
run() {
  if [ -n "$DRY_RUN" ]; then echo "    (dry run) $*"; else "$@"; fi
}
# ask "question" default: the answer, or the default on Enter or without a terminal
ask() {
  if [ -t 0 ]; then
    printf '%s [%s]: ' "$1" "$2" >&2
    read -r answer || answer=""
    echo "${answer:-$2}"
  else
    echo "$2"
  fi
}
yes_no() {
  case $(ask "$1" "$2") in [Yy]*) return 0 ;; *) return 1 ;; esac
}

version=""
action=install
while [ $# -gt 0 ]; do
  case $1 in
    --version)
      [ -n "$2" ] || die "--version needs a version, e.g. 0.4.0"
      version=${2#v}
      shift 2
      ;;
    --uninstall) action=uninstall; shift ;;
    *) echo "usage: $0 [--version X.Y.Z | --uninstall]"; exit 1 ;;
  esac
done

if [ -z "$DRY_RUN" ] && [ "$(id -u)" != 0 ]; then
  die "run it as root: sudo $0"
fi
if [ -z "$DRY_RUN" ] && [ ! -d /run/systemd/system ]; then
  die "this installer needs systemd. Without it, run rradmon from its archive by hand (see the README)."
fi

if [ "$action" = uninstall ]; then
  run systemctl disable --now rradmon 2>/dev/null || true
  rm -f "$UNIT" "$BIN"
  run systemctl daemon-reload
  say "Rradmon removed. Left in place: $DATA (the database), $CONF_DIR (settings) and the user $USER_NAME."
  exit 0
fi

# ---- The program: next to this script, or downloaded
tmp=""
cleanup() { [ -n "$tmp" ] && rm -rf "$tmp"; }
trap cleanup EXIT

if [ -f "$HERE/rradmon" ] && [ -z "$version" ]; then
  src=$HERE/rradmon
else
  # The archive for this machine. A 64-bit kernel can run a 32-bit system
  # (Raspberry Pi OS 32-bit), so ask the system, not the kernel.
  case $(uname -m) in
    x86_64) platform=linux-amd64 ;;
    aarch64 | arm64) platform=linux-arm64 ;;
    armv6* | armv7* | armv8l) platform=linux-armv6 ;;
    *) die "no Rradmon build for $(uname -m)" ;;
  esac
  if [ "$platform" = linux-arm64 ] && [ "$(getconf LONG_BIT 2>/dev/null)" = 32 ]; then
    platform=linux-armv6
  fi
  if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL -o "$2" "$1"; }
    latest() { curl -fsSI "https://github.com/$REPO/releases/latest" | tr -d '\r' | sed -n 's#^[Ll]ocation: .*/tag/v\{0,1\}##p'; }
  elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
    latest() { wget -S --spider --max-redirect=0 "https://github.com/$REPO/releases/latest" 2>&1 | tr -d '\r' | sed -n 's#^ *[Ll]ocation: .*/tag/v\{0,1\}##p'; }
  else
    die "curl or wget is needed to download Rradmon"
  fi
  if [ -z "$version" ]; then
    version=$(latest | tail -n 1)
    [ -n "$version" ] || die "could not find the latest release on GitHub"
  fi
  tmp=$(mktemp -d)
  base=rradmon-$version-$platform
  url=https://github.com/$REPO/releases/download/v$version
  say "Downloading Rradmon $version ($platform)"
  fetch "$url/$base.tar.gz" "$tmp/$base.tar.gz" || die "download failed: $url/$base.tar.gz"
  fetch "$url/rradmon-$version-SHA256SUMS" "$tmp/SHA256SUMS" || die "download failed: SHA256SUMS"
  (cd "$tmp" && grep " $base.tar.gz\$" SHA256SUMS | sha256sum -c --quiet -) || die "checksum mismatch: not installing"
  tar -xzf "$tmp/$base.tar.gz" -C "$tmp"
  src=$tmp/$base/rradmon
fi
new_version=$("$src" version 2>/dev/null | sed 's/^rradmon //') || true

# ---- The user, allowed to use serial ports
if id "$USER_NAME" >/dev/null 2>&1; then
  say "User $USER_NAME exists"
else
  say "Adding user $USER_NAME"
  run useradd -r -M -d "$DATA" -s /usr/sbin/nologin "$USER_NAME"
fi
if getent group dialout >/dev/null 2>&1 && ! id -nG "$USER_NAME" 2>/dev/null | tr ' ' '\n' | grep -qx dialout; then
  run usermod -a -G dialout "$USER_NAME"
fi

# ---- Settings: asked for on a new install
updating=""
[ -f "$UNIT" ] && updating=1
new_conf=""
if [ -f "$CONF" ]; then
  say "Keeping $CONF"
else
  new_conf=1
  echo
  echo "Rradmon settings (Enter keeps the value in brackets; all can be changed later in $CONF):"
  ports=""
  for p in /dev/serial/by-id/* /dev/ttyACM* /dev/ttyUSB*; do
    [ -e "$p" ] && ports="$ports $p"
  done
  [ -n "$ports" ] && echo "  Serial ports found:$ports"
  first_port=$(echo $ports | cut -d' ' -f1)
  serial=$(ask "The RNode's serial port (none if Rradmon only takes observers over MQTT)" "${first_port:-none}")
  [ "$serial" = none ] && serial=""
  freq=$(ask "Frequency, Hz" 925875000)
  bw=$(ask "Bandwidth, Hz" 250000)
  sf=$(ask "Spreading factor" 9)
  cr=$(ask "Coding rate (5 = 4/5)" 5)
  http=127.0.0.1:8080
  if yes_no "Open the web UI to other machines on the network? (y: port 8080 on every interface)" y; then
    http=0.0.0.0:8080
  fi
  options=""
  if yes_no "Run the built-in MQTT broker for observers on this machine (127.0.0.1:1883)?" n; then
    options="-mqtt-listen 127.0.0.1:1883"
  fi
  if [ -z "$serial" ] && [ -z "$options" ]; then
    warn "no serial port and no broker: set -mqtt or -mqtt-listen in RRADMON_OPTIONS in $CONF before starting"
  fi
  mkdir -p "$CONF_DIR"
  cat > "$CONF" <<EOF
# Rradmon settings, read by the rradmon service. After a change:
#   sudo systemctl restart rradmon

# The local RNode's serial port (rradmon ports lists them); empty for none
RRADMON_SERIAL=$serial

# The radio settings of the network to hear
RRADMON_FREQ=$freq
RRADMON_BW=$bw
RRADMON_SF=$sf
RRADMON_CR=$cr

# Web UI: 127.0.0.1:8080 for this machine only, 0.0.0.0:8080 for the network
RRADMON_HTTP=$http

# More options for rradmon run, for example:
#   -mqtt-listen 127.0.0.1:1883               built-in broker, this machine only
#   -mqtt tcp://192.168.1.10:1883 -mqtt-user rradmon -mqtt-pass-file /etc/rradmon/mqtt_password
#   -lat -33.86 -lon 151.21                   the local RNode's position
RRADMON_OPTIONS="$options"
EOF
  chmod 644 "$CONF"
  say "Settings saved in ${CONF#$DESTDIR}"
fi

# ---- Program and service
say "Installing ${BIN#$DESTDIR}${new_version:+ ($new_version)}"
mkdir -p "$(dirname "$BIN")"
cp "$src" "$BIN.new"
chmod 755 "$BIN.new"
mv -f "$BIN.new" "$BIN"

mkdir -p "$(dirname "$UNIT")"
cat > "$UNIT" <<'EOF'
# Installed by Rradmon's install.sh; settings are in /etc/rradmon/rradmon.conf
[Unit]
Description=Rradmon: Reticulum LoRa monitor
After=network-online.target
Wants=network-online.target

[Service]
User=rradmon
EnvironmentFile=/etc/rradmon/rradmon.conf
StateDirectory=rradmon
WorkingDirectory=/var/lib/rradmon
ExecStart=/usr/local/bin/rradmon run -db /var/lib/rradmon/rradmon.db -freq ${RRADMON_FREQ} -bw ${RRADMON_BW} -sf ${RRADMON_SF} -cr ${RRADMON_CR} $RRADMON_OPTIONS
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
run systemctl daemon-reload
run systemctl enable rradmon

if [ -n "$updating" ]; then
  run systemctl restart rradmon
  say "Rradmon updated${new_version:+ to $new_version} and restarted. Log: journalctl -u rradmon -f"
  exit 0
fi
# systemd makes /var/lib/rradmon (StateDirectory) and Rradmon the database
run systemctl start rradmon

# The admin password: needed for the Settings tab, names, deleting nodes
if [ -n "$new_conf" ] && [ -t 0 ]; then
  echo
  if yes_no "Set the admin password now? (needed to change settings in the web UI)" y; then
    [ -n "$DRY_RUN" ] || sleep 2 # let the service create the database first
    run su -s /bin/sh "$USER_NAME" -c "/usr/local/bin/rradmon admin -db $DATA/rradmon.db set-password" || \
      warn "not set; later: sudo -u $USER_NAME rradmon admin -db $DATA/rradmon.db set-password"
  fi
fi

http=$(sed -n 's/^RRADMON_HTTP=//p' "$CONF" | tr -d '"')
port=${http##*:}
case $http in
  127.* | localhost:*) web="http://localhost:$port (this machine only)" ;;
  *)
    addr=$(hostname -I 2>/dev/null | cut -d' ' -f1)
    web="http://${addr:-<this machine>}:$port"
    ;;
esac
cat <<EOF

Rradmon is installed and running.
  Web UI:    $web
  Log:       journalctl -u rradmon -f
  Settings:  ${CONF#$DESTDIR}, then: sudo systemctl restart rradmon
  Admin:     sudo -u $USER_NAME rradmon admin -db $DATA/rradmon.db set-password
  Update:    sudo ./install.sh again (keeps the settings and the database)
EOF
