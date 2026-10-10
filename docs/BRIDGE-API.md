# dsh-mobile-bridge HTTP API v1 (frozen contract)

The Windows bridge (`dsh-mobile-bridge.exe`) runs on the **PC that the phone is physically
attached to**. It owns `adb.exe`, `fastboot.exe`, the adb *server* process, and the COM ports.
The DSH plugin (`harness-pc-mobile` bundle) is an HTTP client of this API.

- Transport: HTTP/1.1, JSON request and response bodies (`Content-Type: application/json`).
- Auth: every `/v1/*` request carries `Authorization: Bearer <token>`. A missing/incorrect
  token is `401` with `{"error":"unauthorized"}`. `/v1/health` **also** requires the token.
- Errors: HTTP status + JSON `{"error":"<message>","code":"<stable_code>"}` where a code exists.
  A child process that exits non-zero is **not** an HTTP error: it is a `200` with `exitCode != 0`.
- Encoding: all raw byte payloads are base64 (standard alphabet, padded) in `data`.
- Limits: request bodies are capped at 64 MiB (chunked uploads use `offset`).
- Version: `GET /v1/health` returns `apiVersion`; this document is `1`.

## Vocabulary

| field | meaning |
|---|---|
| `serial` | an adb/fastboot device serial (what `adb devices` prints in column 1) |
| `port` | a Windows COM port name, e.g. `COM7` |
| `sessionId` | opaque id of an open serial session held by the bridge |

## Command execution result (used by `/v1/adb`, `/v1/fastboot`)

```json
{
  "argv": ["-s", "ABC123", "shell", "getprop", "ro.product.model"],
  "exitCode": 0,
  "stdout": "...",
  "stderr": "...",
  "truncated": false,
  "durationMs": 412
}
```

`stdout`/`stderr` are UTF-8 with invalid sequences replaced. Each stream is capped at 1 MiB;
when capped, `truncated` is `true` and the **tail** is kept. `exitCode` is `-1` and
`error` is set when the process could not be started or timed out.

## Endpoints

### `GET /v1/health`

```json
{
  "apiVersion": 1,
  "ok": true,
  "bridgeVersion": "1.0.0",
  "hostname": "DESKTOP-ABC",
  "os": "windows",
  "arch": "amd64",
  "uptimeSec": 812,
  "platformTools": { "dir": "C:\\...\\platform-tools", "adb": "C:\\...\\adb.exe",
                     "fastboot": "C:\\...\\fastboot.exe", "adbVersion": "Android Debug Bridge version 1.0.41",
                     "source": "config|path|autodownload|missing" },
  "adbServer": { "running": true, "addr": "0.0.0.0:5037", "pid": 1234 },
  "serialPorts": ["COM3", "COM7"],
  "activeSerialSessions": 1
}
```

### `POST /v1/setup`

```json
{ "action": "install_platform_tools" }
```

`action` is one of:

| action | effect |
|---|---|
| `install_platform_tools` | download + unzip Google platform-tools for Windows into the configured dir when adb is missing; idempotent; returns the updated `platformTools` block |
| `start_adb_server` | ensure an adb server is listening on `0.0.0.0:<adbPort>` (kill a 127.0.0.1-only server first if needed); returns the `adbServer` block |
| `stop_adb_server` | `adb kill-server`; returns the `adbServer` block |
| `refresh` | re-resolve platform-tools and re-enumerate; same response shape as `install_platform_tools` plus `serialPorts` |

Response: `{ "ok": true, "message": "...", "health": <the /v1/health object> }`.

### `GET /v1/devices`

```json
{
  "adb": [ { "serial": "ABC123", "state": "device", "model": "Pixel 7",
             "product": "panther", "device": "panther", "transportId": "1" } ],
  "fastboot": [ { "serial": "ABC123", "mode": "fastboot" } ],
  "adbError": null,
  "fastbootError": null,
  "serialPorts": [ { "port": "COM7", "description": "MediaTek USB Port", "inUse": false } ]
}
```

`adb` is parsed from `adb devices -l` (unparsed trailing text is preserved in `extra`).
`fastboot` is parsed from `fastboot devices`. A tool that is unavailable yields the list empty
**and** a non-null `adbError`/`fastbootError` string; that is still `200`.

### `POST /v1/adb` and `POST /v1/fastboot`

```json
{ "args": ["shell", "getprop"], "serial": "ABC123", "timeoutMs": 60000 }
```

- `args` (required, `string[]`): literal argv appended after the optional `-s <serial>`.
  The bridge does **not** shell-interpolate; no quoting rules apply.
- `serial` (optional): when present and non-empty, `-s <serial>` is inserted unless `args`
  already begins with `-s`.
- `timeoutMs` (optional): default `60000`, clamp `1000..1800000`.
- `stdin` (optional, string): written to the child's stdin, then closed.
- Response: the command execution result above.

Fastboot note: `fastboot` has no persistent server, so each call re-enumerates. `serial` is
passed as `-s <serial>`.

### `POST /v1/fs/put` — write a file on the PC

```json
{ "path": "D:\\rom\\boot.img", "data": "<base64>", "offset": 0, "append": false,
  "mkdirs": true }
```

- `offset` (default 0): byte offset to write at. `append: true` means "write at end of file";
  `offset` is ignored then.
- Response: `{ "path": "...", "bytesWritten": 4096, "size": 4096, "sha256": "..." }`
  where `size` is the resulting file size and `sha256` is of the resulting file.

### `POST /v1/fs/get` — read a file from the PC

```json
{ "path": "D:\\rom\\boot.img", "offset": 0, "length": 1048576 }
```

`length` default/`-1` = to end of file, max 8388608 per call. Response:
`{ "path": "...", "offset": 0, "data": "<base64>", "bytes": 1048576, "size": 33554432,
"eof": false, "sha256": "..." }` (`sha256` is of the whole file).

### `POST /v1/fs/ls`

```json
{ "path": "D:\\rom" }
```

Response: `{ "path": "...", "entries": [ { "name": "boot.img", "dir": false, "size": 33554432,
"modified": "2026-10-11T02:00:00Z" } ] }`

### `POST /v1/serial/open`

```json
{ "port": "COM7", "baudRate": 115200, "dataBits": 8, "stopBits": 1,
  "parity": "none", "readTimeoutMs": 500 }
```

`parity` ∈ `none|odd|even|mark|space`; `stopBits` ∈ `1|1.5|2`. Response:
`{ "sessionId": "s1", "port": "COM7", "baudRate": 115200 }`. Opening an already-open port
yields `409` `{"code":"port_busy"}`.

### `POST /v1/serial/write`

```json
{ "sessionId": "s1", "data": "<base64>", "encoding": "base64", "echoNewline": false }
```

`encoding` ∈ `base64|utf8|hex` (default `base64`). `echoNewline: true` appends CRLF after the
payload (line-oriented UART consoles). Response: `{ "sessionId":"s1", "bytesWritten": 16 }`.

### `POST /v1/serial/read`

```json
{ "sessionId": "s1", "timeoutMs": 1000, "maxBytes": 65536 }
```

Blocks up to `timeoutMs` (default 1000, clamp `0..60000`) collecting bytes, returns as soon as
`maxBytes` is reached. Response:
`{ "sessionId":"s1", "data":"<base64>", "bytes":123, "text":"...", "timedOut":true }`.
`text` is the data decoded as UTF-8 with replacement chars; it is always present.

### `POST /v1/serial/close`

```json
{ "sessionId": "s1" }
```

Response: `{ "closed": true }`.

### `GET /v1/serial/ports`

Response: `{ "ports": [ { "port": "COM7", "description": "MediaTek USB Port", "inUse": false } ] }`.

## Bridge configuration

Flags override the config file; the config file (`dsh-mobile-bridge.json`, next to the exe,
or `--config <path>`) overrides defaults.

```json
{
  "listen": "0.0.0.0:8787",
  "token": "change-me",
  "platformToolsDir": "C:\\dsh-mobile\\platform-tools",
  "adbPort": 5037,
  "startAdbServerOnLaunch": true,
  "autoDownloadPlatformTools": true,
  "allowedRoots": ["D:\\", "C:\\Users\\"],
  "logFile": "dsh-mobile-bridge.log"
}
```

`allowedRoots` (optional, default empty = unrestricted) restricts every `path` accepted by
`/v1/fs/*` to those prefixes; a path outside yields `403 {"code":"path_not_allowed"}`.

## Security posture

The bridge executes `adb`/`fastboot` and arbitrary serial I/O on the PC with the current user's
privileges. The token is the only gate, so it must never be a default value on a shared LAN.
Bind to a LAN interface, not the public internet. `fastboot flash|erase` is destructive.
