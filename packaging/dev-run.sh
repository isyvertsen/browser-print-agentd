#!/bin/bash
#
# dev-run.sh — run the agent on THIS Mac, as you, with no installer and no
# administrator password.
#
# It builds the binary, copies it under your own Application Support directory,
# writes a per-user LaunchAgent into ~/Library/LaunchAgents under a `.dev`
# label, and bootstraps it into your GUI session so it starts now and at every
# login. Everything it touches is yours and `--remove` takes all of it away.
#
# What it deliberately does NOT do, compared with the .pkg:
#   - no station certificate and no keychain trust, so there is no :9101 and
#     Safari cannot reach it; Chrome, Edge and Arc use :9100 and need nothing;
#   - no updater;
#   - a different launchd label, so it can never be mistaken for, or collide
#     with, a real install — but it does take the same ports, so uninstall the
#     package (or stop Zebra Browser Print) first.
#
# Usage:
#   packaging/dev-run.sh              # build, install for this user, start
#   packaging/dev-run.sh --remove     # stop and delete everything it made
#   packaging/dev-run.sh --status     # is it running, and what does /health say

set -euo pipefail

PACKAGING_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENT_DIR="$(cd "$PACKAGING_DIR/.." && pwd)"
# shellcheck source=identity.sh
. "$PACKAGING_DIR/identity.sh"

LABEL="${BUNDLE_ID}.dev"
SUPPORT_DIR="$HOME/Library/Application Support/${SUPPORT_DIR_NAME}"
BIN_DIR="$SUPPORT_DIR/bin"
BIN="$BIN_DIR/${BINARY_NAME}"
LOG_DIR="$HOME/Library/Logs/${LOG_DIR_NAME}"
LOG_FILE="$LOG_DIR/${LOG_FILE_NAME}"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"
ORIGINS_FILE="$SUPPORT_DIR/${ORIGINS_FILE_NAME}"
DOMAIN="gui/$(id -u)"
HTTP_PORT=9100
HTTPS_PORT=9101

log() { printf '[dev-run] %s\n' "$*"; }
fail() {
	printf '[dev-run] ERROR: %s\n' "$*" >&2
	exit 1
}

port_holder() {
	# lsof exits 1 when nothing listens; under pipefail that would abort the
	# script inside the command substitution, so the pipeline is made total.
	/usr/sbin/lsof -nP -iTCP:"$1" -sTCP:LISTEN 2>/dev/null | /usr/bin/awk 'NR>1 {print $1, $2}' | head -1 || true
}

status() {
	if /bin/launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
		log "$LABEL is registered in $DOMAIN"
	else
		log "$LABEL is not registered"
	fi
	if curl -fsS --max-time 2 "http://127.0.0.1:$HTTP_PORT/health" 2>/dev/null; then
		echo
	else
		log "nothing answers on http://127.0.0.1:$HTTP_PORT/health"
		holder="$(port_holder $HTTP_PORT)"
		[ -z "$holder" ] || log "port $HTTP_PORT is held by: $holder"
	fi
	[ -f "$LOG_FILE" ] && log "log: $LOG_FILE" || true
}

remove() {
	/bin/launchctl bootout "$DOMAIN/$LABEL" >/dev/null 2>&1 || true
	/bin/rm -f "$PLIST"
	/bin/rm -rf "$BIN_DIR"
	log "removed $LABEL, $PLIST and $BIN_DIR"
	log "kept $ORIGINS_FILE and $LOG_DIR (delete by hand if you want them gone)"
}

case "${1:-}" in
--status)
	status
	exit 0
	;;
--remove)
	remove
	exit 0
	;;
"") ;;
*)
	fail "unknown argument: $1 (expected nothing, --status or --remove)"
	;;
esac

[ "$(uname -s)" = "Darwin" ] || fail "this is a macOS LaunchAgent; nothing to do here"
command -v go >/dev/null 2>&1 || fail "go is not on PATH (brew install go)"
for tool in lp lpstat; do
	command -v "$tool" >/dev/null 2>&1 || fail "$tool not found; the agent refuses to start without CUPS"
done

# The agent would crash-loop behind KeepAlive if something else has the port,
# so refuse up front and name the holder — the same bargain preinstall makes.
/bin/launchctl bootout "$DOMAIN/$LABEL" >/dev/null 2>&1 || true
sleep 1
for port in $HTTP_PORT $HTTPS_PORT; do
	holder="$(port_holder $port)"
	[ -z "$holder" ] || fail "port $port is held by $holder; stop it first (Zebra Browser Print, or the installed package: sudo ${UNINSTALLER_NAME})"
done

log "building $BINARY_NAME"
mkdir -p "$BIN_DIR" "$LOG_DIR"
(cd "$AGENT_DIR" && CGO_ENABLED=0 go build -trimpath -buildvcs=false \
	-ldflags "-X main.version=dev-$(git -C "$AGENT_DIR" rev-parse --short HEAD 2>/dev/null || echo local)" \
	-o "$BIN" ./...)
chmod 755 "$BIN"

if [ ! -e "$ORIGINS_FILE" ]; then
	{
		printf '# Origins allowed to print through this agent, one per line.\n'
		printf '# Example:  https://labels.example.com\n'
		printf '# A lone * allows every origin (upstream behaviour; not recommended).\n'
		printf '# The running agent re-reads this file when it changes.\n'
	} >"$ORIGINS_FILE"
	chmod 600 "$ORIGINS_FILE"
	log "wrote an empty $ORIGINS_FILE"
fi

mkdir -p "$(dirname "$PLIST")"
cat >"$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>${LABEL}</string>
	<key>ProgramArguments</key>
	<array>
		<string>${BIN}</string>
		<string>--bind</string>
		<string>127.0.0.1</string>
		<string>--port</string>
		<string>${HTTP_PORT}</string>
		<string>--https-port</string>
		<string>${HTTPS_PORT}</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin</string>
		<key>${LOG_PATH_ENV}</key>
		<string>${LOG_FILE}</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
EOF
/usr/bin/plutil -lint "$PLIST" >/dev/null || fail "$PLIST is not a valid property list"

log "bootstrapping $LABEL into $DOMAIN"
/bin/launchctl bootstrap "$DOMAIN" "$PLIST"
/bin/launchctl kickstart -k "$DOMAIN/$LABEL" >/dev/null 2>&1 || true

attempt=0
until curl -fsS --max-time 2 "http://127.0.0.1:$HTTP_PORT/available" >/dev/null 2>&1; do
	attempt=$((attempt + 1))
	if [ "$attempt" -ge 15 ]; then
		/usr/bin/tail -20 "$LOG_FILE" >&2 2>/dev/null || true
		fail "the agent did not answer on port $HTTP_PORT; see $LOG_FILE"
	fi
	sleep 1
done

log "running: http://127.0.0.1:$HTTP_PORT (no HTTPS listener in dev mode)"
log "allow your web app: echo 'https://your.app' >> '$ORIGINS_FILE'"
log "status: $0 --status    remove: $0 --remove"
status
