#!/bin/bash
# Detached, self-healing restart of the DSH web process inside the fnOS "Harness" app.
#
# Why detached: this script's parent is the very process it restarts, so it has to
# survive that process dying. Start it with setsid+nohup (see "usage" below) and it
# logs to .gotmp/restart.log.
#
# Why a fallback: the fnOS wrapper (`/vol1/@appcenter/Harness/bin/harness`, holds
# /vol1/@appcenter/Harness/app.sock) supervises `dsh web`; killing the child is expected
# to make the wrapper spawn a fresh one. If it does not within ~90s, this script
# relaunches `dsh web` itself from the captured cmdline/cwd/env, so the panel at :13080
# comes back either way.
#
# Everything is discovered at arm time — nothing is hard-coded, because the child pid
# changes on every restart.
#
# usage:
#   cd "/vol1/1000/Ai workspaces/deepseek harness/harness-pc-mobile"
#   setsid nohup bash tools/restart-harness.sh >/dev/null 2>&1 &
#   # then poll: tail -f .gotmp/restart.log
set -u

ROOT=/vol1/1000/Ai workspaces/deepseek harness/harness-pc-mobile
GOTMP="$ROOT/.gotmp"
LOG="$GOTMP/restart.log"
mkdir -p "$GOTMP"

exec >>"$LOG" 2>&1
echo "================================================================"
echo "[restart] armed $(date -Is)"

probe() {
  curl -sS --noproxy '*' -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:13080/" 2>/dev/null
}

# --- discover the `dsh web` child by scanning /proc (no pgrep dependency) -----------
# Matching on the raw cmdline alone is NOT safe: any shell command that merely mentions
# "dsh web … --port 13080" (e.g. a grep, or this script's own invocation) would match.
# So require the process to actually BE a node binary whose argv runs the dsh entrypoint.
CHILD=""
for d in /proc/[0-9]*; do
  pid="${d#/proc/}"
  exe="$(readlink "$d/exe" 2>/dev/null)" || continue
  case "${exe##*/}" in node | nodejs) ;; *) continue ;; esac
  cmd="$(tr '\0' ' ' <"$d/cmdline" 2>/dev/null)"
  case "$cmd" in
    *"node_modules/.bin/dsh"*"web"*"--port 13080"*) CHILD="$pid" ;;
  esac
done

if [ -z "$CHILD" ]; then
  echo "[restart] ABORT: no node 'dsh web --port 13080' process found; nothing to restart"
  exit 1
fi

PPID_OF="$(awk '{print $4}' "/proc/$CHILD/stat" 2>/dev/null)"
PPID_EXE="$(readlink "/proc/$PPID_OF/exe" 2>/dev/null)"
echo "[restart] target child=$CHILD exe=$exe ppid=$PPID_OF ($PPID_EXE) http=$(probe)"
case "$PPID_EXE" in
  */Harness/bin/harness) ;;
  *) echo "[restart] WARNING: parent is not the fnOS Harness wrapper — verify before trusting the respawn fallback" ;;
esac

# Capture how to relaunch it ourselves, in case the supervisor does not.
readlink "/proc/$CHILD/cwd" >"$GOTMP/child.cwd" 2>/dev/null
readlink "/proc/$CHILD/exe" >"$GOTMP/child.exe" 2>/dev/null
tr '\0' '\n' <"/proc/$CHILD/environ" 2>/dev/null \
  | grep -E '^[A-Za-z_][A-Za-z0-9_]*=' \
  | sed "s/'/'\\\\''/g; s/^/export '/; s/$/'/" >"$GOTMP/child.env" 2>/dev/null
echo "[restart] captured cwd=$(cat "$GOTMP/child.cwd" 2>/dev/null) env=$(wc -l <"$GOTMP/child.env" 2>/dev/null) lines"

# Give the caller's last HTTP response time to flush before we cut the process off.
sleep 12

if kill -0 "$CHILD" 2>/dev/null; then
  echo "[restart] TERM $CHILD"
  kill -TERM "$CHILD" 2>/dev/null
else
  echo "[restart] child $CHILD was already gone"
fi

for i in $(seq 1 45); do
  sleep 2
  code="$(probe)"
  if [ "$code" = "200" ] || [ "$code" = "401" ]; then
    echo "[restart] panel is back after $((i * 2))s (http=$code) — supervisor respawned it"
    exit 0
  fi
done

echo "[restart] supervisor did not respawn within 90s — relaunching 'dsh web' manually"
NODE="$(cat "$GOTMP/child.exe" 2>/dev/null)"
[ -x "$NODE" ] || NODE=/vol1/@appcenter/nodejs_v24/bin/node
CWD="$(cat "$GOTMP/child.cwd" 2>/dev/null)"
if [ -n "$CWD" ]; then cd "$CWD" || cd /vol1/@appcenter/Harness/server; else cd /vol1/@appcenter/Harness/server || exit 1; fi
set -a
# shellcheck disable=SC1090
. "$GOTMP/child.env"
set +a
nohup "$NODE" /vol1/@appcenter/Harness/server/node_modules/.bin/dsh web --no-open --port 13080 >>"$LOG" 2>&1 &
echo "[restart] manual relaunch pid $! (cwd=$CWD node=$NODE)"

for i in $(seq 1 30); do
  sleep 2
  code="$(probe)"
  if [ "$code" = "200" ] || [ "$code" = "401" ]; then
    echo "[restart] manual relaunch is serving after $((i * 2))s (http=$code)"
    exit 0
  fi
done
echo "[restart] FAILED: :13080 still not answering — start Harness from the fnOS app centre"
exit 1
