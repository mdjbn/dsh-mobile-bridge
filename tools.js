/**
 * The seven `pc_mobile_*` tools.
 *
 * These are the agent's whole interface to the phone. Every one of them is a
 * thin, explicit wrapper over one HTTP call to `dsh-mobile-bridge.exe` running
 * on the Windows PC; nothing here talks to a phone directly. Two ideas carry
 * most of the design:
 *
 *   1. The PC is the only machine that can see the phone. adb, fastboot, the
 *      COM ports and the ROM image files all live there, so an argument that is
 *      a PATH is always a PC path (`pc_mobile_file` moves server files over).
 *   2. A tool that would destroy something asks first. `fastboot flash`,
 *      `erase` and `format` refuse to run without `confirm: true`, while plain
 *      adb stays ungated because nothing in adb is irreversible by itself.
 *
 * @module dsh-pc-mobile-bridge/tools
 */

import { createHash } from 'node:crypto'
import { createReadStream } from 'node:fs'
import { open } from 'node:fs/promises'

import { hostAdbStatus, installHostAdb } from './host-adb.js'

/** Every tool renders its value as pretty JSON; the model reads that. */
export function renderJson(_args, value) {
  return [{ type: 'text', text: JSON.stringify(value, null, 2) }]
}

/**
 * The output schema every tool shares. It is deliberately permissive: the
 * bridge's payloads are the contract (`docs/BRIDGE-API.md`), and duplicating
 * them here would only create a second place to drift.
 */
const OPEN_OUTPUT = {
  schema: { type: 'object', additionalProperties: true },
  render: renderJson,
}

function requireString(args, field) {
  const value = args?.[field]
  if (typeof value !== 'string' || value.trim() === '') {
    throw new Error(`\`${field}\` is required and must be a non-empty string`)
  }
  return value.trim()
}

function optionalString(args, field) {
  const value = args?.[field]
  if (value === undefined || value === null) return undefined
  if (typeof value !== 'string') throw new Error(`\`${field}\` must be a string`)
  return value.trim() === '' ? undefined : value.trim()
}

function requireArgv(args) {
  const value = args?.args
  if (!Array.isArray(value) || value.length === 0) {
    throw new Error('`args` is required and must be a non-empty array of strings, e.g. ["devices", "-l"]')
  }
  for (const item of value) {
    if (typeof item !== 'string') {
      throw new Error(`every entry of \`args\` must be a string; got ${JSON.stringify(item)}`)
    }
  }
  return value
}

function optionalPositiveInt(args, field, { min = 1, max = Number.MAX_SAFE_INTEGER } = {}) {
  const value = args?.[field]
  if (value === undefined || value === null) return undefined
  if (!Number.isInteger(value) || value < min || value > max) {
    throw new Error(`\`${field}\` must be an integer between ${min} and ${max}`)
  }
  return value
}

/** The subcommand of an argv, i.e. the first entry that is not a global flag. */
function subcommand(argv) {
  for (const item of argv) {
    if (!item.startsWith('-')) return item.toLowerCase()
  }
  return ''
}

/**
 * Does this fastboot argv write to the device? Returns a human reason, or
 * undefined when the command only reads state. The reason is quoted back to the
 * model so a refusal explains itself.
 */
export function fastbootRisk(argv) {
  const command = subcommand(argv)
  if (['flash', 'flashall', 'erase', 'format', 'update', 'flashing', 'delete', 'resize-logical-partition',
    'delete-logical-partition', 'create-logical-partition', 'snapshot-update'].includes(command)) {
    return `\`fastboot ${command}\` writes to the device`
  }
  if (argv.some(item => item === '-w' || item === '--wipe-and-use-fbe')) {
    return 'the `-w` flag wipes userdata'
  }
  if (command === 'oem') {
    const action = argv.find(item => !item.startsWith('-') && item.toLowerCase() !== 'oem')?.toLowerCase() ?? ''
    if (['unlock', 'unlock_critical', 'lock', 'lock_critical', 'flashing', 'format', 'erase', 'frp'].includes(action)) {
      return `\`fastboot oem ${action}\` changes device state irreversibly`
    }
  }
  if (command === 'set_active' || command === '--set-active') {
    return '`set_active` changes which slot the device boots from'
  }
  return undefined
}

/** Hash a file on this server, streaming, so a large ROM never lands in memory. */
async function sha256File(path) {
  const hash = createHash('sha256')
  for await (const chunk of createReadStream(path)) hash.update(chunk)
  return hash.digest('hex')
}

/**
 * Build every tool. `client` is one configured `BridgeClient`; `config` is the
 * resolved plugin config (it also tells `pc_mobile_setup` where adb lives on
 * THIS server when routing host adb through the PC).
 */
export function createTools(client, config) {
  const pcMobileStatus = {
    name: 'pc_mobile_status',
    description: 'Report the state of the bridge that connects this DSH server to the phone: whether '
      + 'dsh-mobile-bridge.exe answers, where its platform-tools are, whether its adb server is listening, which '
      + 'COM ports exist, and whether this server\'s own adb is routed to the PC. Run this FIRST when anything '
      + 'phone-related misbehaves — every other pc_mobile_* tool fails with a raw transport error when the bridge '
      + 'is down or the token is wrong. Set `startAdbServer: true` before the first adb or fastboot work if the '
      + 'report shows adbServer.running false.',
    parameters: {
      startAdbServer: {
        type: 'boolean',
        description: 'Also run the bridge\'s `start_adb_server` setup action, which launches `adb -a -P <port> '
          + 'nodaemon server` on the PC and reports the refreshed state. Do this once per PC boot.',
      },
      includeDevices: {
        type: 'boolean',
        description: 'Also list adb/fastboot devices and COM ports, i.e. run pc_mobile_devices in the same call.',
      },
    },
    output: OPEN_OUTPUT,
    timeoutMs: 60_000,
    isConcurrencySafe: () => true,
    async execute(args) {
      const health = await client.health()
      let setup
      if (args?.startAdbServer === true) {
        setup = await client.setup('start_adb_server', { timeoutMs: 120_000 })
      }
      const bridge = setup?.health ?? health
      let devices
      if (args?.includeDevices === true) devices = await client.devices()
      const hostAdb = await safeHostAdb(config)
      return {
        bridgeUrl: config.bridgeUrl,
        reachable: true,
        bridge: {
          ok: bridge.ok,
          apiVersion: bridge.apiVersion,
          bridgeVersion: bridge.bridgeVersion,
          hostname: bridge.hostname,
          os: bridge.os,
          arch: bridge.arch,
          listen: bridge.listen,
          uptimeSec: bridge.uptimeSec,
        },
        platformTools: bridge.platformTools,
        adbServer: bridge.adbServer,
        serialPorts: bridge.serialPorts,
        activeSerialSessions: bridge.activeSerialSessions,
        hostAdb,
        ...(setup === undefined ? {} : { setup }),
        ...(devices === undefined ? {} : { devices }),
        next:
          bridge.adbServer?.running === true
            ? 'the PC adb server is up — use pc_mobile_devices or pc_mobile_adb'
            : 'the PC adb server is NOT running; call pc_mobile_status with startAdbServer: true (or run the '
              + 'pc_mobile_setup action "start_adb_server") before adb work',
      }
    },
    presentCall: () => ({ card: 'generic', title: 'PC bridge status' }),
  }

  const pcMobileDevices = {
    name: 'pc_mobile_devices',
    description: 'List every Android target the PC can currently see: adb devices with their serial, state and '
      + 'model, fastboot devices, and the PC\'s COM ports. adb devices in state "device" are ready; "unauthorized" '
      + 'needs the USB-debugging prompt accepted ON the phone; "offline" usually means it is still booting. Note '
      + 'that a phone sitting in MTK/Qualcomm DOWNLOAD mode is invisible to both adb and fastboot — it only shows '
      + 'up as a COM port, which is what pc_mobile_serial is for. A device in real fastboot/bootloader mode shows '
      + 'up under fastboot and NOT under adb.',
    parameters: {},
    output: OPEN_OUTPUT,
    timeoutMs: 90_000,
    isConcurrencySafe: () => true,
    async execute() {
      const listed = await client.devices({ timeoutMs: 60_000 })
      const adb = Array.isArray(listed.adb) ? listed.adb : []
      const fastboot = Array.isArray(listed.fastboot) ? listed.fastboot : []
      const serialPorts = Array.isArray(listed.serialPorts) ? listed.serialPorts : []
      const notes = []
      if (listed.adbError !== null && listed.adbError !== undefined) notes.push(`adb: ${listed.adbError}`)
      if (listed.fastbootError !== null && listed.fastbootError !== undefined) {
        notes.push(`fastboot: ${listed.fastbootError}`)
      }
      if (adb.length === 0 && fastboot.length === 0 && notes.length === 0) {
        notes.push('no device over adb or fastboot. If the phone is in download/flash mode, use the COM port from '
          + '`serialPorts` with pc_mobile_serial; otherwise check the cable carries data, USB debugging is on, and '
          + 'the PC adb server was started (pc_mobile_status with startAdbServer: true).')
      }
      return {
        adb,
        fastboot,
        serialPorts,
        adbError: listed.adbError ?? null,
        fastbootError: listed.fastbootError ?? null,
        ready: adb.filter(entry => entry.state === 'device').map(entry => entry.serial),
        notes,
      }
    },
    presentCall: () => ({ card: 'generic', title: 'List PC-visible Android devices' }),
  }

  const pcMobileAdb = {
    name: 'pc_mobile_adb',
    description: 'Run one adb command ON THE PC — the machine the phone is plugged into — and return its stdout, '
      + 'stderr and exit code. `args` is a literal argv array and never a shell string: ["shell", "getprop", '
      + '"ro.product.model"], not "shell getprop ro.product.model". Pick a device with `serial` (or put "-s", '
      + '"<serial>" at the front of `args` yourself) whenever more than one is attached. Reading commands are safe; '
      + '`adb shell` runs with shell privileges on the phone, so `rm`, `dd`, `pm clear`, `fastboot`, `flash` and '
      + '`reboot bootloader` really do change the phone — state what you intend before running them. A command that '
      + 'times out or fails to launch comes back with exitCode -1 and an `error` field, not as a tool failure.',
    parameters: {
      args: {
        type: 'array',
        items: { type: 'string' },
        required: true,
        description: 'The adb argv, e.g. ["devices", "-l"], ["shell", "getprop", "ro.build.version.release"], '
          + '["reboot", "bootloader"]. Do not include the word "adb" itself.',
      },
      serial: {
        type: 'string',
        description: 'Device serial from pc_mobile_devices, injected as `-s <serial>` before the rest of argv.',
      },
      timeoutMs: {
        type: 'integer',
        description: 'How long the PC waits for adb, in milliseconds (default 60000, max 1800000). Use a larger '
          + 'value for `adb pull`/`push` of a whole ROM, `adb logcat -d` on a busy device, or `adb wait-for-device`.',
      },
      stdin: {
        type: 'string',
        description: 'Text piped to adb\'s stdin, for the few commands that read it (e.g. ["shell"] with a heredoc).',
      },
    },
    output: OPEN_OUTPUT,
    timeoutMs: 1_860_000,
    isConcurrencySafe: () => true,
    async execute(args) {
      const argv = requireArgv(args)
      const result = await client.adb(argv, {
        serial: optionalString(args, 'serial'),
        timeoutMs: optionalPositiveInt(args, 'timeoutMs', { min: 1000, max: 1_800_000 }),
        stdin: optionalString(args, 'stdin'),
      })
      return summarize(result)
    },
    presentCall: args => ({ card: 'generic', title: `adb ${Array.isArray(args?.args) ? args.args.join(' ') : ''}`.trim() }),
  }

  const pcMobileFastboot = {
    name: 'pc_mobile_fastboot',
    description: 'Run one fastboot command on the PC against a phone in fastboot/bootloader mode. fastboot has no '
      + 'daemon: each call re-enumerates USB, so the phone must already be in fastboot mode (`adb reboot '
      + 'bootloader`) and a phone in download mode showing a COM port is NOT visible here. Paths in `args` are PC '
      + 'paths and must already exist there — use pc_mobile_file to put a ROM image on the PC first. Commands that '
      + 'write to the device (`flash`, `erase`, `format`, `update`, `-w`, `oem unlock`, `set_active`) are refused '
      + 'unless `confirm: true` is set, and the refusal names the command so you can reconsider.',
    parameters: {
      args: {
        type: 'array',
        items: { type: 'string' },
        required: true,
        description: 'The fastboot argv, e.g. ["devices"], ["getvar", "current-slot"], ["flash", "boot", '
          + '"C:\\\\roms\\\\boot.img"]. Do not include the word "fastboot" itself.',
      },
      serial: {
        type: 'string',
        description: 'Device serial from pc_mobile_devices, injected as `-s <serial>`.',
      },
      timeoutMs: {
        type: 'integer',
        description: 'How long the PC waits, in milliseconds (default 60000). Flashing a large partition over USB 2 '
          + 'regularly takes several minutes — raise this for `flash`, `update` or `flashall`.',
      },
      confirm: {
        type: 'boolean',
        description: 'Required to be true for the destructive commands. Set it only after you have verified the '
          + 'phone, the partition name and the image file, and only when the user asked for that write.',
      },
    },
    output: OPEN_OUTPUT,
    timeoutMs: 1_860_000,
    isConcurrencySafe: () => true,
    async execute(args) {
      const argv = requireArgv(args)
      const risk = fastbootRisk(argv)
      if (risk !== undefined && args?.confirm !== true) {
        throw new Error(`refusing to run \`fastboot ${argv.join(' ')}\`: ${risk}. Re-issue the call with `
          + '`confirm: true` once the target device (pc_mobile_devices), the partition and the image file are '
          + 'verified — this write cannot be undone.')
      }
      const result = await client.fastboot(argv, {
        serial: optionalString(args, 'serial'),
        timeoutMs: optionalPositiveInt(args, 'timeoutMs', { min: 1000, max: 1_800_000 }),
      })
      return { ...summarize(result), ...(risk === undefined ? {} : { confirmed: true, risk }) }
    },
    presentCall: args => ({
      card: 'generic',
      title: `fastboot ${Array.isArray(args?.args) ? args.args.join(' ') : ''}`.trim(),
    }),
  }

  const pcMobileFile = {
    name: 'pc_mobile_file',
    description: 'Move files between this DSH server and the PC, or inspect PC paths. The flashing workflow needs '
      + 'this: `fastboot flash` and `fastboot update` read files that exist ON THE PC, so a ROM image sitting on '
      + 'this server must be uploaded first. op "upload" copies `localPath` (this server) to `remotePath` (the PC) '
      + 'in chunks and reports both SHA-256 digests so a bad transfer is visible; "download" is the reverse; "ls" '
      + 'lists a PC directory with sizes; "sha256" hashes a file that is already on the PC and returns nothing else '
      + '(cheap, for confirming a ROM matches a known digest). Large ROMs are moved in chunks and never buffered '
      + 'whole, so a multi-gigabyte image is fine.',
    parameters: {
      op: {
        type: 'string',
        required: true,
        enum: ['upload', 'download', 'ls', 'sha256'],
        description: 'upload: server → PC. download: PC → server. ls: list a PC directory. sha256: hash a PC file.',
      },
      localPath: {
        type: 'string',
        description: 'Path on THIS server. Required for upload (the source) and download (the destination), ignored '
          + 'by ls/sha256.',
      },
      remotePath: {
        type: 'string',
        required: true,
        description: 'Path on the PC: the destination for upload, the source for download, the directory for ls, '
          + 'the file for sha256. Use a PC-style path (e.g. C:\\\\roms\\\\boot.img). The PC side refuses paths '
          + 'outside the allowedRoots configured in dsh-mobile-bridge.json.',
      },
      chunkBytes: {
        type: 'integer',
        description: 'Transfer chunk size in bytes (default 4194304). Lower it only if the PC bridge complains '
          + 'about memory or the request-body limit.',
      },
    },
    output: OPEN_OUTPUT,
    timeoutMs: 3_600_000,
    isConcurrencySafe: () => false,
    async execute(args) {
      const op = requireString(args, 'op')
      const remotePath = requireString(args, 'remotePath')
      const chunkBytes = optionalPositiveInt(args, 'chunkBytes', { min: 65_536, max: 64 * 1024 * 1024 })
        ?? Math.min(config.chunkBytes, 8 * 1024 * 1024)

      if (op === 'ls') {
        const listing = await client.fsLs({ path: remotePath })
        return { op, ...listing }
      }
      if (op === 'sha256') {
        // /v1/fs/get reports the WHOLE file's digest regardless of the slice it
        // returns, so one byte is enough to hash an arbitrarily large image.
        const probe = await client.fsGet({ path: remotePath, offset: 0, length: 1 })
        return { op, path: probe.path, size: probe.size, sha256: probe.sha256 }
      }
      if (op === 'upload') {
        const localPath = requireString(args, 'localPath')
        const digest = await sha256File(localPath)
        const handle = await open(localPath, 'r')
        try {
          let offset = 0
          let last
          for (;;) {
            const buffer = Buffer.allocUnsafe(chunkBytes)
            const { bytesRead } = await handle.read(buffer, 0, chunkBytes, offset)
            if (bytesRead === 0 && offset > 0) break
            last = await client.fsPut({
              path: remotePath,
              data: buffer.subarray(0, bytesRead).toString('base64'),
              offset,
              mkdirs: true,
            }, { timeoutMs: 600_000 })
            offset += bytesRead
            if (bytesRead < chunkBytes) break
          }
          const verified = last?.sha256 === digest
          return {
            op,
            localPath,
            remotePath: last?.path ?? remotePath,
            bytes: last?.size ?? offset,
            chunks: Math.max(1, Math.ceil(offset / chunkBytes)),
            localSha256: digest,
            remoteSha256: last?.sha256,
            verified,
            ...(verified ? {} : {
              error: 'the PC reports a different digest than this server — the upload is corrupt or incomplete; '
                + 're-run it and check the PC disk',
            }),
          }
        } finally {
          await handle.close()
        }
      }
      if (op === 'download') {
        const localPath = requireString(args, 'localPath')
        const handle = await open(localPath, 'w')
        try {
          let offset = 0
          let total = 0
          let last
          for (;;) {
            const slice = await client.fsGet({ path: remotePath, offset, length: chunkBytes }, { timeoutMs: 600_000 })
            const buffer = Buffer.from(slice.data ?? '', 'base64')
            if (buffer.length > 0) await handle.write(buffer, 0, buffer.length, offset)
            offset += buffer.length
            total += buffer.length
            last = slice
            if (slice.eof === true) break
            if (buffer.length === 0) break
          }
          await handle.close()
          const digest = await sha256File(localPath)
          const verified = last?.sha256 === digest
          return {
            op,
            remotePath,
            localPath,
            bytes: total,
            expectedSize: last?.size,
            localSha256: digest,
            remoteSha256: last?.sha256,
            verified,
            ...(verified ? {} : { error: 'the downloaded file does not hash to the PC\'s digest — re-run it' }),
          }
        } catch (error) {
          await handle.close().catch(() => {})
          throw error
        }
      }
      throw new Error(`unknown op ${JSON.stringify(op)} — use one of upload, download, ls, sha256`)
    },
    presentCall: args => ({ card: 'generic', title: `PC file ${args?.op ?? ''}: ${args?.remotePath ?? ''}`.trim() }),
  }

  const pcMobileSerial = {
    name: 'pc_mobile_serial',
    description: 'Read and write the PC\'s serial ports (COM ports). This is the tool for a phone that is NOT '
      + 'reachable over adb or fastboot: the UART console on a debug header, and the USB-VCOM port that MTK '
      + 'download mode, Qualcomm EDL and vendor flash tools expose. Sessions live in the bridge and are addressed '
      + 'by sessionId, so the flow is always open → read/write as many times as needed → close; an open port is '
      + 'held until closed (or until DSH\'s bridge restarts), so close it when you are done or the flash tool on '
      + 'the PC will find the port busy. Reading drains whatever arrived during `timeoutMs` and returns as soon as '
      + 'the line goes quiet, which is what makes it usable against an interactive console.',
    parameters: {
      action: {
        type: 'string',
        required: true,
        enum: ['list', 'open', 'write', 'read', 'close'],
        description: 'list: every COM port with its description. open: take the port and return a sessionId. '
          + 'write: send bytes. read: drain what arrived. close: release the port.',
      },
      port: {
        type: 'string',
        description: 'COM port name for `open`, exactly as `list` reports it — "COM5" on Windows.',
      },
      baudRate: {
        type: 'integer',
        description: 'Baud rate for `open` (default 115200). UART consoles are usually 115200; MTK download mode '
          + 'often runs at 921600 or 460800.',
      },
      dataBits: { type: 'integer', description: 'Data bits for `open`: 5, 6, 7 or 8 (default 8).' },
      stopBits: { type: 'integer', description: 'Stop bits for `open`: 1 or 2 (default 1).' },
      parity: {
        type: 'string',
        enum: ['none', 'odd', 'even', 'mark', 'space'],
        description: 'Parity for `open` (default none).',
      },
      sessionId: {
        type: 'string',
        description: 'The session returned by `open`. Required for read/write/close.',
      },
      data: {
        type: 'string',
        description: 'Payload for `write`. With encoding "utf8" this is plain text ("reboot\\n"); with "hex" it is '
          + 'e.g. "7e0001"; with "base64" it is base64.',
      },
      encoding: {
        type: 'string',
        enum: ['utf8', 'hex', 'base64'],
        description: 'How to interpret `data` on write (default utf8). Reads always return base64 plus a decoded '
          + 'utf8 `text` field.',
      },
      echoNewline: {
        type: 'boolean',
        description: 'On write, append CR LF after the payload — a bootloader console usually needs it to accept a '
          + 'command.',
      },
      timeoutMs: {
        type: 'integer',
        description: 'On read, how long to keep draining before returning, in milliseconds (default 2000, max '
          + '60000). It returns early once the port goes quiet, so a larger value only matters for slow output.',
      },
      maxBytes: {
        type: 'integer',
        description: 'On read, stop after this many bytes (default 65536, max 4194304).',
      },
    },
    output: OPEN_OUTPUT,
    timeoutMs: 120_000,
    isConcurrencySafe: () => false,
    async execute(args) {
      const action = requireString(args, 'action')
      switch (action) {
        case 'list': {
          const ports = await client.serialPorts()
          return { action, ...ports }
        }
        case 'open': {
          const options = {
            port: requireString(args, 'port'),
            baudRate: optionalPositiveInt(args, 'baudRate', { min: 50, max: 12_000_000 }),
            dataBits: optionalPositiveInt(args, 'dataBits', { min: 5, max: 8 }),
            stopBits: optionalPositiveInt(args, 'stopBits', { min: 1, max: 2 }),
            parity: optionalString(args, 'parity'),
          }
          const opened = await client.serialOpen(clean(options), { timeoutMs: 30_000 })
          return {
            action,
            ...opened,
            hint: 'keep this sessionId; read it with action "read" and release it with action "close" when the '
              + 'console work is done, otherwise a PC-side flash tool will find the port busy',
          }
        }
        case 'write': {
          const payload = {
            sessionId: requireString(args, 'sessionId'),
            data: requireString(args, 'data'),
            encoding: optionalString(args, 'encoding'),
            echoNewline: args?.echoNewline === true,
          }
          const written = await client.serialWrite(clean(payload), { timeoutMs: 30_000 })
          return { action, ...written }
        }
        case 'read': {
          const payload = {
            sessionId: requireString(args, 'sessionId'),
            timeoutMs: optionalPositiveInt(args, 'timeoutMs', { min: 0, max: 60_000 }),
            maxBytes: optionalPositiveInt(args, 'maxBytes', { min: 1, max: 4_194_304 }),
          }
          const read = await client.serialRead(clean(payload), { timeoutMs: 90_000 })
          return {
            action,
            ...read,
            ...(read.bytes === 0 && read.timedOut === true
              ? { hint: 'nothing arrived on the port within the timeout — check the baud rate, and note that a port '
                  + 'can be open and silent until the phone prints something' }
              : {}),
          }
        }
        case 'close': {
          const closed = await client.serialClose({ sessionId: requireString(args, 'sessionId') }, { timeoutMs: 30_000 })
          return { action, ...closed }
        }
        default:
          throw new Error(`unknown action ${JSON.stringify(action)} — use list, open, write, read or close`)
      }
    },
    presentCall: args => ({ card: 'generic', title: `PC serial ${args?.action ?? ''}`.trim() }),
  }

  const pcMobileSetup = {
    name: 'pc_mobile_setup',
    description: 'Prepare the PC bridge and this server. Actions: "install_platform_tools" downloads and unzips '
      + 'Google\'s platform-tools on the PC when adb/fastboot are missing (needs internet on the PC); '
      + '"start_adb_server" launches the PC\'s adb server bound to 0.0.0.0 so this server can reach it; '
      + '"stop_adb_server" stops it; "refresh" re-probes platform-tools and the adb server without changing '
      + 'anything; "install_host_adb" routes THIS SERVER\'s own adb at the PC (a wrapper at $HOME/Android/Sdk/'
      + 'platform-tools/adb exporting ADB_SERVER_SOCKET=tcp:<pc>:<port>), which is what makes the built-in '
      + 'android_* tools drive the PC-attached phone — it needs a restart of DSH to take effect for those tools.',
    parameters: {
      action: {
        type: 'string',
        required: true,
        enum: ['install_platform_tools', 'start_adb_server', 'stop_adb_server', 'refresh', 'install_host_adb'],
        description: 'Which preparation step to run.',
      },
      force: {
        type: 'boolean',
        description: 'For install_host_adb: re-download the Linux platform-tools client even when a working one is '
          + 'already staged.',
      },
      clientPath: {
        type: 'string',
        description: 'For install_host_adb: path to an existing adb binary on THIS server to route instead of '
          + 'downloading one.',
      },
    },
    output: OPEN_OUTPUT,
    timeoutMs: 600_000,
    isConcurrencySafe: () => false,
    async execute(args) {
      const action = requireString(args, 'action')
      if (action === 'install_host_adb') {
        const url = new URL(config.bridgeUrl)
        const installed = await installHostAdb(config, {
          host: url.hostname,
          port: config.adbServerPort,
          force: args?.force === true,
          clientPath: optionalString(args, 'clientPath'),
        })
        return { action, ...installed }
      }
      if (!['install_platform_tools', 'start_adb_server', 'stop_adb_server', 'refresh'].includes(action)) {
        throw new Error(`unknown action ${JSON.stringify(action)} — use install_platform_tools, start_adb_server, `
          + 'stop_adb_server, refresh or install_host_adb')
      }
      const result = await client.setup(action, { timeoutMs: 300_000 })
      return { action, ...result, hostAdb: await safeHostAdb(config) }
    },
    presentCall: args => ({ card: 'generic', title: `PC bridge setup: ${args?.action ?? ''}`.trim() }),
  }

  return [
    pcMobileStatus,
    pcMobileDevices,
    pcMobileAdb,
    pcMobileFastboot,
    pcMobileFile,
    pcMobileSerial,
    pcMobileSetup,
  ].map(withJsonSchemaParameters)
}

/**
 * Compile this file's author-facing `parameters` literals into the raw JSON
 * Schema that `ctx.tools.register()` requires.
 *
 * The harness's own `defineTool()` does exactly this — it turns the
 * per-property dialect (`{ args: { type: 'array', required: true } }`) into
 * `{ type: 'object', properties, required: [...] }` — but `defineTool` lives in
 * `@deepseek-ai/dsh-tools`, which is not resolvable from this profile's
 * `node_modules`; this plugin stays dependency-free and carries the equivalent.
 *
 * Registering the author dialect verbatim ships the model a function whose
 * schema has no `type`, which the API rejects before the turn even starts:
 *   Invalid schema for function 'pc_mobile_adb': schema must be a JSON Schema of
 *   'type: "object"', got 'type: null'.
 *
 * Only the top level is compiled because every `required` in this file sits
 * directly under `parameters` (nested `items` are already plain JSON Schema).
 * `tests/schema-conformance.mjs` pins this against the harness's own
 * `parameterSchemaSpecToJsonSchema` so the two cannot drift.
 *
 * @param spec - author-facing per-property parameter definitions.
 * @returns An object-rooted raw JSON Schema.
 */
export function compileParameterSpec(spec) {
  const properties = {}
  const required = []
  for (const [key, node] of Object.entries(spec ?? {})) {
    const { required: isRequired, ...rest } = node
    properties[key] = rest
    if (isRequired === true) required.push(key)
  }
  return {
    type: 'object',
    properties,
    ...(required.length === 0 ? {} : { required }),
  }
}

/** Attach a compiled `parameters` schema to one tool definition. */
function withJsonSchemaParameters(tool) {
  return {
    ...tool,
    parameters: compileParameterSpec(tool.parameters),
  }
}

/** Drop undefined fields so request bodies match the documented contract. */
function clean(object) {
  return Object.fromEntries(Object.entries(object).filter(([, value]) => value !== undefined))
}

/** The compact shape the model reads for a finished command. */
function summarize(result) {
  const clip = (value, max = 24_000) => {
    const text = typeof value === 'string' ? value : ''
    if (text.length <= max) return text
    return `${text.slice(text.length - max)}\n…[earlier output clipped]`
  }
  return {
    argv: Array.isArray(result.argv) ? result.argv : [],
    exitCode: typeof result.exitCode === 'number' ? result.exitCode : -1,
    stdout: clip(result.stdout),
    stderr: clip(result.stderr),
    ...(result.truncated === true ? { outputClipped: true } : {}),
    ...(typeof result.error === 'string' && result.error !== '' ? { error: result.error } : {}),
    durationMs: result.durationMs,
  }
}

/** Host-adb inspection never fails a status call; it reports the failure. */
async function safeHostAdb(config) {
  try {
    return await hostAdbStatus(config)
  } catch (error) {
    return { error: `could not inspect the host adb routing: ${error?.message ?? error}` }
  }
}
