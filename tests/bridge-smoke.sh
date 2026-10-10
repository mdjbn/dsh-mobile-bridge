#!/usr/bin/env bash
# Contract smoke test for dsh-mobile-bridge.
#
# Runs on this Linux server, where no adb/fastboot exists: it therefore proves
# the HTTP contract, the auth gate, the raw-filesystem chunking and the serial
# session bookkeeping, and proves that a missing adb degrades into a structured
# result instead of an HTTP failure.  The Windows-only behaviour (real adb,
# platform-tools auto-download, COM ports) is exercised on the PC itself.
#
# Every check asserts an exact observable value; any failure prints the body and
# exits non-zero, so the test cannot report success it did not observe.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/dist/dsh-mobile-bridge"
WORK="$ROOT/bridge/.gotmp/smoke"
TOKEN="smoke-token-0123456789"
FAILURES=0
CHECKS=0

# Never inherit a proxy: a stray http_proxy would silently redirect every
# request to another service and make the whole run meaningless.
CURL=(curl -s --noproxy '*')

pick_free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

PORT="${PORT:-$(pick_free_port)}"
BASE="http://127.0.0.1:$PORT"

rm -rf "$WORK"; mkdir -p "$WORK/data"
ALLOWED="$WORK/data"
RANDOM_BIN="$WORK/data/random.bin"

cat > "$WORK/config.json" <<JSON
{
  "listen": "127.0.0.1:$PORT",
  "token": "$TOKEN",
  "platformToolsDir": "$WORK/absent-platform-tools",
  "adbPort": 15037,
  "startAdbServerOnLaunch": false,
  "autoDownloadPlatformTools": false,
  "allowedRoots": ["$ALLOWED"],
  "logFile": "$WORK/bridge.log"
}
JSON

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; CHECKS=$((CHECKS + 1)); }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILURES=$((FAILURES + 1)); CHECKS=$((CHECKS + 1)); }

check() { # check <label> <actual> <expected>
  if [ "$2" = "$3" ]; then pass "$1 ($2)"; else fail "$1: got '$2', want '$3'"; fi
}

jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

post() { "${CURL[@]}" -m 60 -X POST "$BASE$1" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d "$2"; }
get()  { "${CURL[@]}" -m 30 "$BASE$1" -H "Authorization: Bearer $TOKEN"; }
code() { "${CURL[@]}" -o /dev/null -w '%{http_code}' -m 20 "$@"; }

chmod +x "$BIN" 2>/dev/null
"$BIN" --config "$WORK/config.json" > "$WORK/stdout.log" 2>&1 &
BRIDGE_PID=$!
trap 'kill $BRIDGE_PID 2>/dev/null; wait $BRIDGE_PID 2>/dev/null' EXIT

# Wait for the bridge's own banner line, not merely for *something* to answer:
# another service may already own the port we picked.
READY=0
for _ in $(seq 1 100); do
  kill -0 "$BRIDGE_PID" 2>/dev/null || break
  if grep -q 'bridgeUrl' "$WORK/stdout.log" 2>/dev/null; then READY=1; break; fi
  sleep 0.2
done

if [ "$READY" != 1 ]; then
  echo "bridge did not start on $BASE — its log says:"
  cat "$WORK/stdout.log"
  echo "1 CHECK(S) FAILED"
  exit 1
fi
echo "bridge up on $BASE (pid $BRIDGE_PID)"
echo

echo "== auth =="
check "no token -> 401"       "$(code "$BASE/v1/health")" 401
check "wrong token -> 401"    "$(code "$BASE/v1/health" -H 'Authorization: Bearer wrong')" 401
check "bad scheme -> 401"     "$(code "$BASE/v1/health" -H "Authorization: $TOKEN")" 401

echo "== health =="
HEALTH=$(get /v1/health)
check "health ok"             "$(echo "$HEALTH" | jget 'd["ok"]')" True
check "health apiVersion"     "$(echo "$HEALTH" | jget 'd["apiVersion"]')" 1
check "health os"             "$(echo "$HEALTH" | jget 'd["os"]')" linux
check "health arch"           "$(echo "$HEALTH" | jget 'd["arch"]')" amd64
check "health state"          "$(echo "$HEALTH" | jget '"bridgeVersion" in d and d["ok"] is True')" True
check "platformTools.source"  "$(echo "$HEALTH" | jget 'd["platformTools"]["source"]')" missing
check "platformTools.adb"     "$(echo "$HEALTH" | jget 'd["platformTools"]["adb"]')" ""
check "platformTools.adbError" "$(echo "$HEALTH" | jget '"not found" in d["platformTools"]["adbError"]')" True
check "adbServer.running"     "$(echo "$HEALTH" | jget 'd["adbServer"]["running"]')" False
check "adbServer.addr is the configured one" "$(echo "$HEALTH" | jget 'd["adbServer"]["addr"]')" "127.0.0.1:15037"
check "serialPorts is list"   "$(echo "$HEALTH" | jget 'isinstance(d["serialPorts"],list)')" True
check "activeSerialSessions"  "$(echo "$HEALTH" | jget 'd["activeSerialSessions"]')" 0
check "listen advertised"     "$(echo "$HEALTH" | jget 'd["listen"]')" "127.0.0.1:$PORT"

echo "== devices (no adb on this host) =="
DEV=$(get /v1/devices)
check "devices adb empty"         "$(echo "$DEV" | jget 'len(d["adb"])')" 0
check "devices adbError set"      "$(echo "$DEV" | jget 'bool(d["adbError"])')" True
check "devices fastbootError set" "$(echo "$DEV" | jget 'bool(d["fastbootError"])')" True
check "devices fastboot empty"    "$(echo "$DEV" | jget 'len(d["fastboot"])')" 0

echo "== adb/fastboot argv shaping (observable without adb installed) =="
ADB_RESP=$(post /v1/adb '{"args":["version"]}')
check "missing adb -> exitCode -1"     "$(echo "$ADB_RESP" | jget 'd["exitCode"]')" -1
check "missing adb explains itself"    "$(echo "$ADB_RESP" | jget 'bool(d.get("error"))')" True
check "argv echoed"                    "$(echo "$ADB_RESP" | jget 'd["argv"]==["version"]')" True
SERIAL_RESP=$(post /v1/adb '{"args":["devices","-l"],"serial":"ABC123"}')
check "serial is injected as -s"       "$(echo "$SERIAL_RESP" | jget 'd["argv"]==["-s","ABC123","devices","-l"]')" True
EXPLICIT=$(post /v1/adb '{"args":["-s","ZZZ","shell","id"],"serial":"ABC123"}')
check "explicit -s is not duplicated"  "$(echo "$EXPLICIT" | jget 'd["argv"]==["-s","ZZZ","shell","id"]')" True
FB=$(post /v1/fastboot '{"args":["devices"]}')
check "fastboot argv echoed"           "$(echo "$FB" | jget 'd["argv"]==["devices"]')" True

echo "== request validation =="
check "empty args -> 400"      "$(code -X POST "$BASE/v1/adb" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d '{"args":[]}')" 400
check "args wrong type -> 400" "$(code -X POST "$BASE/v1/adb" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d '{"args":"devices"}')" 400
check "malformed json -> 400"  "$(code -X POST "$BASE/v1/adb" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d '{oops')" 400
check "GET on a POST route -> 405" "$(code "$BASE/v1/adb" -H "Authorization: Bearer $TOKEN")" 405
check "unknown route -> 404"   "$(code "$BASE/v1/nope" -H "Authorization: Bearer $TOKEN")" 404
BAD=$(post /v1/setup '{"action":"nope"}')
check "unknown setup action named" "$(echo "$BAD" | jget '"unknown setup action" in json.dumps(d)')" True

echo "== fs: 300 KiB chunked round trip =="
head -c 307200 /dev/urandom > "$RANDOM_BIN"
SRC_SHA=$(sha256sum "$RANDOM_BIN" | cut -d' ' -f1)

REMOTE="$WORK/data/uploaded.bin"
UPLOAD=$(python3 - "$RANDOM_BIN" "$BASE" "$TOKEN" "$REMOTE" <<'PY'
import base64, hashlib, json, sys, urllib.request
src, base, token, remote = sys.argv[1:5]
raw = open(src, 'rb').read()
chunk = 128 * 1024
offset, last = 0, None
while offset < len(raw):
    part = raw[offset:offset + chunk]
    body = json.dumps({"path": remote, "data": base64.b64encode(part).decode(),
                       "offset": offset, "mkdirs": True}).encode()
    req = urllib.request.Request(base + "/v1/fs/put", data=body, method="POST",
                                 headers={"Authorization": "Bearer " + token,
                                          "Content-Type": "application/json"})
    last = json.load(urllib.request.urlopen(req, timeout=60))
    offset += len(part)
print(last["sha256"] == hashlib.sha256(raw).hexdigest(), last["size"], last["bytesWritten"])
PY
)
check "upload: per-chunk sha256 == whole file" "$(echo "$UPLOAD" | cut -d' ' -f1)" True
check "upload: final size"                     "$(echo "$UPLOAD" | cut -d' ' -f2)" 307200
check "upload: last chunk bytesWritten"        "$(echo "$UPLOAD" | cut -d' ' -f3)" 45056

GET1=$(post /v1/fs/get "{\"path\":\"$REMOTE\"}")
check "get: whole-file sha256 matches local" "$(echo "$GET1" | jget 'd["sha256"]')" "$SRC_SHA"
check "get: default length reads to EOF"     "$(echo "$GET1" | jget 'd["eof"]')" True
check "get: bytes == file size"              "$(echo "$GET1" | jget 'd["bytes"]')" 307200

# Download back in 1 MiB slices, exactly like the plugin does.
DOWNLOADED="$WORK/downloaded.bin"
: > "$DOWNLOADED"
OFFSET=0
GUARD=0
while :; do
  GUARD=$((GUARD + 1))
  if [ "$GUARD" -gt 64 ]; then fail "download loop did not terminate after 64 slices"; break; fi
  SLICE=$(post /v1/fs/get "{\"path\":\"$REMOTE\",\"offset\":$OFFSET,\"length\":1048576}")
  echo "$SLICE" | python3 -c "
import base64, json, sys
d = json.load(sys.stdin)
if 'data' not in d:
    sys.stderr.write(json.dumps(d) + '\n'); sys.exit(3)
sys.stdout.buffer.write(base64.b64decode(d['data']))
" >> "$DOWNLOADED" || { fail "download slice at offset $OFFSET: $(echo "$SLICE" | head -c 200)"; break; }
  EOF_FLAG=$(echo "$SLICE" | jget 'd["eof"]')
  [ "$EOF_FLAG" = "True" ] && break
  OFFSET=$((OFFSET + 1048576))
done
check "download: bytes identical to source" "$(sha256sum < "$DOWNLOADED" | cut -d' ' -f1)" "$SRC_SHA"

echo "== fs: listing and containment =="
LS=$(post /v1/fs/ls "{\"path\":\"$WORK/data\"}")
check "ls sees uploaded file"    "$(echo "$LS" | jget 'any(e["name"]=="uploaded.bin" for e in d["entries"])')" True
check "ls marks it a regular file" "$(echo "$LS" | jget 'not [e for e in d["entries"] if e["name"]=="uploaded.bin"][0]["dir"]')" True
check "ls reports its size"      "$(echo "$LS" | jget '[e for e in d["entries"] if e["name"]=="uploaded.bin"][0]["size"]')" 307200
check "ls of a missing dir -> 404"   "$(code -X POST "$BASE/v1/fs/ls" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d "{\"path\":\"$WORK/data/nope\"}")" 404
check "get of a missing file -> 404" "$(code -X POST "$BASE/v1/fs/get" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d "{\"path\":\"$WORK/data/nope\"}")" 404
check "outside allowedRoots -> 403"  "$(code -X POST "$BASE/v1/fs/ls" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d '{"path":"/etc"}')" 403
check "traversal out of allowedRoots -> 403" "$(code -X POST "$BASE/v1/fs/ls" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d "{\"path\":\"$ALLOWED/../../..\"}")" 403
check "sibling prefix is not containment -> 403" "$(code -X POST "$BASE/v1/fs/ls" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d "{\"path\":\"$ALLOWED-evil\"}")" 403

echo "== serial sessions =="
PORTS=$(get /v1/serial/ports)
check "serial ports is a list" "$(echo "$PORTS" | jget 'isinstance(d["ports"],list)')" True
OPEN=$(post /v1/serial/open '{"port":"/dev/does-not-exist","baudRate":115200}')
check "opening a missing port is explained" "$(echo "$OPEN" | jget '"cannot open" in d.get("error","")')" True
check "no session leaked after a failed open" "$(get /v1/health | jget 'd["activeSerialSessions"]')" 0
check "unknown session read -> 404"  "$(code -X POST "$BASE/v1/serial/read" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d '{"sessionId":"s99","timeoutMs":200}')" 404
check "unknown session write -> 404" "$(code -X POST "$BASE/v1/serial/write" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d '{"sessionId":"s99","data":"00","encoding":"hex"}')" 404
check "unknown session close -> 404" "$(code -X POST "$BASE/v1/serial/close" -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -d '{"sessionId":"s99"}')" 404

echo
echo "$CHECKS checks, $FAILURES failure(s)"
if [ "$FAILURES" -eq 0 ]; then
  echo "ALL CHECKS PASSED"
else
  echo "$FAILURES CHECK(S) FAILED"
fi
exit "$FAILURES"
