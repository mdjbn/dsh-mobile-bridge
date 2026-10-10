/**
 * Plugin smoke test: exercises the real tool objects against the real bridge
 * binary on this Linux host.
 *
 * This is the closest thing to an end-to-end check that can run without a
 * phone: the tools, the HTTP client, the auth gate, the argv shaping, the
 * chunked file transfer and the destructive-command gate are all the same code
 * the model will run. adb and fastboot are absent here on purpose — that is
 * itself part of the contract (a missing tool must degrade into a structured
 * result, never an HTTP failure), and the serial/COM-port path cannot be
 * exercised without a port, so its bookkeeping is checked through the error
 * paths only.
 *
 * Run: node tests/plugin-smoke.mjs
 */

import { spawn } from 'node:child_process'
import { mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { existsSync } from 'node:fs'
import { createServer } from 'node:net'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { BridgeClient, resolveConfig } from '../bridge-client.js'
import { apply } from '../index.js'
import { createTools, fastbootRisk } from '../tools.js'

const here = dirname(fileURLToPath(import.meta.url))
const root = join(here, '..')
const BIN = join(root, 'dist', 'dsh-mobile-bridge')
const WORK = join(root, 'bridge', '.gotmp', 'plugin-smoke')
const TOKEN = 'plugin-smoke-token'

let failures = 0
let checks = 0

function check(label, actual, expected = true) {
  checks += 1
  if (actual === expected) {
    console.log(`  \x1b[32mPASS\x1b[0m ${label} (${String(actual)})`)
  } else {
    failures += 1
    console.log(`  \x1b[31mFAIL\x1b[0m ${label}: got ${JSON.stringify(actual)}, want ${JSON.stringify(expected)}`)
  }
}

async function freePort() {
  return new Promise(resolve => {
    const server = createServer()
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address()
      server.close(() => resolve(port))
    })
  })
}

async function main() {
  if (!existsSync(BIN)) {
    console.error(`missing ${BIN} — build it first (see README.md)`)
    process.exit(1)
  }
  await rm(WORK, { recursive: true, force: true })
  await mkdir(join(WORK, 'data'), { recursive: true })

  const port = await freePort()
  const configPath = join(WORK, 'config.json')
  await writeFile(configPath, JSON.stringify({
    listen: `127.0.0.1:${port}`,
    token: TOKEN,
    platformToolsDir: join(WORK, 'absent'),
    adbPort: 15038,
    startAdbServerOnLaunch: false,
    autoDownloadPlatformTools: false,
    allowedRoots: [join(WORK, 'data')],
    logFile: join(WORK, 'bridge.log'),
  }, null, 2))

  const child = spawn(BIN, ['--config', configPath], { stdio: ['ignore', 'pipe', 'pipe'] })
  let bridgeLog = ''
  child.stdout.on('data', chunk => { bridgeLog += chunk.toString() })
  child.stderr.on('data', chunk => { bridgeLog += chunk.toString() })

  const base = `http://127.0.0.1:${port}`
  const deadline = Date.now() + 20_000
  let up = false
  while (Date.now() < deadline) {
    if (child.exitCode !== null) break
    if (bridgeLog.includes('bridgeUrl')) { up = true; break }
    await new Promise(resolve => setTimeout(resolve, 200))
  }
  if (!up) {
    console.error(`the bridge did not start on ${base}:\n${bridgeLog}`)
    child.kill()
    process.exit(1)
  }
  console.log(`bridge up on ${base}`)

  const config = resolveConfig({ bridgeUrl: base, token: TOKEN })
  const client = new BridgeClient(config)
  const tools = Object.fromEntries(createTools(client, config).map(tool => [tool.name, tool]))

  const byName = name => {
    const tool = tools[name]
    if (tool === undefined) throw new Error(`tool ${name} was not created`)
    return tool
  }

  try {
    console.log('\n== registration surface ==')
    check('creates the seven pc_mobile_* tools', Object.keys(tools).length, 7)
    check('every tool declares output.render',
      Object.values(tools).every(tool => typeof tool.output?.render === 'function'))
    check('every tool has a name + description + parameters',
      Object.values(tools).every(tool => typeof tool.name === 'string'
        && typeof tool.description === 'string' && tool.description.length > 80
        && typeof tool.parameters === 'object'))
    check('no tool name collides with the android_* namespace',
      Object.keys(tools).every(name => name.startsWith('pc_mobile_')))

    // ctx.tools.register() takes COMPILED JSON Schema, not this plugin's
    // author-facing per-property dialect. Registering the dialect sends the API
    // a function whose schema root has no `type`, which kills the whole turn:
    //   Invalid schema for function 'pc_mobile_adb': schema must be a JSON
    //   Schema of 'type: "object"', got 'type: null'.
    check('every parameters schema is object-rooted JSON Schema',
      Object.values(tools).every(tool => tool.parameters?.type === 'object'
        && typeof tool.parameters.properties === 'object'
        && (tool.parameters.required === undefined || Array.isArray(tool.parameters.required))))
    check('no author-dialect `required: true` leaf survived',
      Object.values(tools).every(tool => !JSON.stringify(tool.parameters).includes('"required":true')))
    check('every required entry names a declared property',
      Object.values(tools).every(tool => (tool.parameters.required ?? [])
        .every(key => Object.hasOwn(tool.parameters.properties, key))))
    check('pc_mobile_adb required list', byName('pc_mobile_adb').parameters.required.join(','), 'args')
    check('pc_mobile_devices compiles to an empty object schema',
      JSON.stringify(byName('pc_mobile_devices').parameters), '{"type":"object","properties":{}}')
    check('output schemas stay object-rooted too',
      Object.values(tools).every(tool => tool.output?.schema?.type === 'object'))

    console.log('\n== apply() registers through the harness service ==')
    const registered = []
    const fakeCtx = {
      tools: { register: tool => { registered.push(tool.name); return () => {} } },
      effect: fn => { fn(); return () => {} },
      logger: { info: () => {}, warn: () => {} },
    }
    const dispose = apply(fakeCtx, { bridgeUrl: base, token: TOKEN })
    check('apply registered 7 tools', registered.length, 7)
    check('apply returned a disposer', typeof dispose, 'function')
    check('registered names', registered.join(','),
      'pc_mobile_status,pc_mobile_devices,pc_mobile_adb,pc_mobile_fastboot,pc_mobile_file,pc_mobile_serial,pc_mobile_setup')

    console.log('\n== an unconfigured client explains itself instead of failing transport ==')
    const bare = new BridgeClient(resolveConfig({}))
    const bareTools = Object.fromEntries(createTools(bare, resolveConfig({})).map(tool => [tool.name, tool]))
    let unconfigured
    try {
      await bareTools.pc_mobile_status.execute({})
    } catch (error) {
      unconfigured = error.message
    }
    check('unconfigured status throws', typeof unconfigured === 'string')
    check('...and names the fix (cordis.patch.yml)', unconfigured.includes('cordis.patch.yml'))

    console.log('\n== pc_mobile_status ==')
    const status = await byName('pc_mobile_status').execute({})
    check('reachable', status.reachable, true)
    check('bridgeUrl echoed', status.bridgeUrl, base)
    check('bridge.os is the PC bridge OS', status.bridge.os, 'linux')
    check('platformTools.source', status.platformTools.source, 'missing')
    check('adbServer.running is false (never started)', status.adbServer.running, false)
    check('hostAdb is reported', typeof status.hostAdb === 'object')
    check('next step says to start the adb server', status.next.includes('start_adb_server'), true)
    const withDevices = await byName('pc_mobile_status').execute({ includeDevices: true })
    check('includeDevices adds a devices block', Array.isArray(withDevices.devices.adb), true)

    console.log('\n== pc_mobile_devices ==')
    const devices = await byName('pc_mobile_devices').execute({})
    check('adb list is empty', devices.adb.length, 0)
    check('adbError explains the missing binary', /adb/.test(devices.adbError), true)
    check('fastbootError explains the missing binary', /fastboot/.test(devices.fastbootError), true)
    check('serialPorts is a list', Array.isArray(devices.serialPorts), true)
    check('notes are surfaced', Array.isArray(devices.notes) && devices.notes.length > 0, true)

    console.log('\n== pc_mobile_adb ==')
    const version = await byName('pc_mobile_adb').execute({ args: ['version'] })
    check('missing adb -> exitCode -1', version.exitCode, -1)
    check('missing adb -> error field', typeof version.error === 'string' && version.error.length > 0, true)
    check('argv is echoed', version.argv.join(' '), 'version')
    const withSerial = await byName('pc_mobile_adb').execute({ args: ['devices', '-l'], serial: 'ABC' })
    check('serial injected as -s', withSerial.argv.join(' '), '-s ABC devices -l')
    let badArgv
    try {
      await byName('pc_mobile_adb').execute({ args: [] })
    } catch (error) {
      badArgv = error.message
    }
    check('empty args is refused locally', badArgv.includes('non-empty array'), true)
    let badType
    try {
      await byName('pc_mobile_adb').execute({ args: ['shell', 42] })
    } catch (error) {
      badType = error.message
    }
    check('non-string argv entry is refused locally', badType.includes('must be a string'), true)

    console.log('\n== pc_mobile_fastboot destructive gate ==')
    check('flash is a risk', typeof fastbootRisk(['flash', 'boot', 'boot.img']) === 'string', true)
    check('erase is a risk', typeof fastbootRisk(['erase', 'userdata']) === 'string', true)
    check('-w is a risk', typeof fastbootRisk(['-w']) === 'string', true)
    check('oem unlock is a risk', typeof fastbootRisk(['oem', 'unlock']) === 'string', true)
    check('getvar is not a risk', fastbootRisk(['getvar', 'current-slot']) === undefined, true)
    check('devices is not a risk', fastbootRisk(['devices']) === undefined, true)
    check('oem device-info is not a risk', fastbootRisk(['oem', 'device-info']) === undefined, true)
    let refused
    try {
      await byName('pc_mobile_fastboot').execute({ args: ['flash', 'boot', 'boot.img'] })
    } catch (error) {
      refused = error.message
    }
    check('unconfirmed flash is refused', refused.includes('confirm: true'), true)
    check('the refusal names the command', refused.includes('fastboot flash boot boot.img'), true)
    check('the refusal says why', refused.includes('writes to the device'), true)
    const safe = await byName('pc_mobile_fastboot').execute({ args: ['devices'] })
    check('a read-only fastboot call goes through', safe.argv.join(' '), 'devices')

    console.log('\n== pc_mobile_file ==')
    const localSource = join(WORK, 'rom.img')
    const payload = Buffer.alloc(700_000)
    for (let index = 0; index < payload.length; index += 1) payload[index] = (index * 31) & 0xff
    await writeFile(localSource, payload)
    const remote = join(WORK, 'data', 'rom.img')
    const uploaded = await byName('pc_mobile_file').execute({
      op: 'upload', localPath: localSource, remotePath: remote, chunkBytes: 262_144,
    })
    check('upload verified', uploaded.verified, true)
    check('upload size', uploaded.bytes, 700_000)
    check('upload used three chunks', uploaded.chunks, 3)
    check('upload digests agree', uploaded.localSha256, uploaded.remoteSha256)
    const hashed = await byName('pc_mobile_file').execute({ op: 'sha256', remotePath: remote })
    check('sha256 of a PC file matches', hashed.sha256, uploaded.localSha256)
    check('sha256 reports the size', hashed.size, 700_000)
    const listed = await byName('pc_mobile_file').execute({ op: 'ls', remotePath: join(WORK, 'data') })
    check('ls finds the ROM', listed.entries.some(entry => entry.name === 'rom.img'), true)
    const localCopy = join(WORK, 'rom-copy.img')
    const downloaded = await byName('pc_mobile_file').execute({
      op: 'download', remotePath: remote, localPath: localCopy, chunkBytes: 262_144,
    })
    check('download verified', downloaded.verified, true)
    check('download bytes', downloaded.bytes, 700_000)
    check('downloaded bytes are identical',
      Buffer.compare(await readFile(localCopy), payload), 0)
    let badOp
    try {
      await byName('pc_mobile_file').execute({ op: 'teleport', remotePath: remote })
    } catch (error) {
      badOp = error.message
    }
    check('unknown op names the alternatives', badOp.includes('upload, download, ls, sha256'), true)
    let outside
    try {
      await byName('pc_mobile_file').execute({ op: 'ls', remotePath: '/etc' })
    } catch (error) {
      outside = error.message
    }
    check('path outside allowedRoots is rejected by the PC', outside.includes('HTTP 403'), true)

    console.log('\n== pc_mobile_serial ==')
    const ports = await byName('pc_mobile_serial').execute({ action: 'list' })
    check('list returns a ports array', Array.isArray(ports.ports), true)
    let openFailed
    try {
      await byName('pc_mobile_serial').execute({ action: 'open', port: 'COM404', baudRate: 115200 })
    } catch (error) {
      openFailed = error.message
    }
    check('opening a nonexistent port reports why', /cannot open|HTTP 400/.test(openFailed), true)
    let noSession
    try {
      await byName('pc_mobile_serial').execute({ action: 'read', sessionId: 's99' })
    } catch (error) {
      noSession = error.message
    }
    check('a stale session is a 404, not a 400', noSession.includes('HTTP 404'), true)
    let badAction
    try {
      await byName('pc_mobile_serial').execute({ action: 'yodel' })
    } catch (error) {
      badAction = error.message
    }
    check('unknown action names the alternatives', badAction.includes('list, open, write, read or close'), true)

    console.log('\n== pc_mobile_setup ==')
    // On this host adb genuinely does not exist, so `refresh` must report
    // ok:false WITH a reason — that is the honest answer, not a bug.
    const refreshed = await byName('pc_mobile_setup').execute({ action: 'refresh' })
    check('refresh answers', typeof refreshed.message, 'string')
    check('refresh reports adb is unavailable here', refreshed.ok === false && /adb/.test(refreshed.message), true)
    check('refresh returns the action', refreshed.action, 'refresh')
    check('refresh carries the fresh health payload', refreshed.health.apiVersion, 1)
    check('refresh includes host adb status', typeof refreshed.hostAdb === 'object')
    const stopped = await byName('pc_mobile_setup').execute({ action: 'stop_adb_server' })
    check('stop_adb_server succeeds even when none is running', stopped.ok, true)
    let badSetup
    try {
      await byName('pc_mobile_setup').execute({ action: 'nope' })
    } catch (error) {
      badSetup = error.message
    }
    check('unknown setup action names the alternatives', badSetup.includes('install_platform_tools'), true)

    console.log('\n== the bridge never saw a malformed request ==')
    const log = await readFile(join(WORK, 'bridge.log'), 'utf8').catch(() => '')
    check('bridge log has no panic', /panic/i.test(log), false)
  } finally {
    child.kill('SIGTERM')
    await new Promise(resolve => child.on('exit', resolve))
  }

  console.log(`\n${checks} checks, ${failures} failure(s)`)
  if (failures === 0) {
    console.log('ALL CHECKS PASSED')
  } else {
    console.log(`${failures} CHECK(S) FAILED`)
  }
  process.exit(failures === 0 ? 0 : 1)
}

await main()
