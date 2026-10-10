/**
 * HTTP client for `dsh-mobile-bridge.exe` — the Windows-side helper that owns
 * adb, fastboot and the phone's COM ports.
 *
 * The frozen wire contract lives in `docs/BRIDGE-API.md` in this workspace and
 * in the bridge package itself. Everything here is one thin layer over
 * `fetch`: bearer auth, a request timeout, and error bodies turned into
 * messages an agent can act on.
 *
 * @module dsh-pc-mobile-bridge/bridge-client
 */

/** Defaults applied when the profile patch config omits a field. */
export const DEFAULT_CONFIG = {
  bridgeUrl: '',
  token: '',
  requestTimeoutMs: 120_000,
  /** Upload chunk size for `pc_mobile_file` transfers, in bytes. */
  chunkBytes: 4 * 1024 * 1024,
  /** adb server port the PC bridge exposes for host-side adb routing. */
  adbServerPort: 5037,
  /** `install_host_adb` writes this file; dsh-android probes it via $HOME/Android/Sdk. */
  hostSdkPath: 'Android/Sdk',
  /** Linux platform-tools archive used by `install_host_adb`. */
  platformToolsUrl: 'https://dl.google.com/android/repository/platform-tools-latest-linux.zip',
}

/** Normalize raw plugin config into the shape the client and tools use. */
export function resolveConfig(raw) {
  const config = { ...DEFAULT_CONFIG, ...(raw ?? {}) }
  const envUrl = process.env.DSH_PC_MOBILE_BRIDGE_URL?.trim()
  const envToken = process.env.DSH_PC_MOBILE_BRIDGE_TOKEN?.trim()
  let bridgeUrl = typeof config.bridgeUrl === 'string' ? config.bridgeUrl.trim() : ''
  if (bridgeUrl === '' && envUrl !== undefined && envUrl !== '') bridgeUrl = envUrl
  let token = typeof config.token === 'string' ? config.token.trim() : ''
  if (token === '' && envToken !== undefined && envToken !== '') token = envToken
  if (bridgeUrl !== '' && !/^https?:\/\//i.test(bridgeUrl)) bridgeUrl = `http://${bridgeUrl}`
  bridgeUrl = bridgeUrl.replace(/\/+$/, '')
  return { ...config, bridgeUrl, token }
}

/** The error every tool throws when the profile patch has no bridge configured. */
export const UNCONFIGURED_MESSAGE = 'dsh-pc-mobile-bridge: no bridge is configured. Add an override entry to your '
  + 'profile\'s cordis.patch.yml:\n'
  + '  - id: pc-mobile-bridge\n'
  + '    name: "dsh-pc-mobile-bridge"\n'
  + '    config:\n'
  + '      bridgeUrl: "http://<pc-lan-ip>:8787"\n'
  + '      token: "<token printed by dsh-mobile-bridge.exe>"\n'
  + '(or set DSH_PC_MOBILE_BRIDGE_URL and DSH_PC_MOBILE_BRIDGE_TOKEN). A config-only change is applied live and '
  + 'needs no restart; restart DSH only if the plugin code itself changed.'

/** A bridge call that failed: transport, status, or a non-JSON body. */
export class BridgeError extends Error {
  constructor(message, { status, code, body } = {}) {
    super(message)
    this.name = 'BridgeError'
    this.status = status
    this.code = code
    this.body = body
  }
}

function isJsonObject(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/** One configured bridge connection. Stateless apart from its config. */
export class BridgeClient {
  constructor(config) {
    this.config = config
    this.base = config.bridgeUrl
    this.token = config.token
  }

  get configured() {
    return this.base !== '' && this.token !== ''
  }

  /** Throw the actionable configuration error, or return the base URL. */
  requireConfigured() {
    if (this.base === '') throw new BridgeError(UNCONFIGURED_MESSAGE)
    if (this.token === '') throw new BridgeError(`${UNCONFIGURED_MESSAGE}\n(the token is missing)`)
    return this.base
  }

  /**
   * One JSON request. `body` is JSON-encoded when defined; `timeoutMs`
   * defaults to the configured request timeout.
   */
  async request(method, path, { body, timeoutMs, raw } = {}) {
    this.requireConfigured()
    const budget = timeoutMs ?? this.config.requestTimeoutMs
    let response
    try {
      response = await fetch(`${this.base}${path}`, {
        method,
        headers: {
          authorization: `Bearer ${this.token}`,
          ...(body === undefined ? {} : { 'content-type': 'application/json' }),
        },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
        signal: AbortSignal.timeout(budget),
      })
    } catch (error) {
      const cause = error?.cause?.code ?? error?.code ?? ''
      const hint = cause === 'ECONNREFUSED'
        ? ' — is dsh-mobile-bridge.exe running on the PC and is port 8787 reachable from this server?'
        : error?.name === 'TimeoutError' ? ` — the bridge did not answer within ${budget} ms` : ''
      throw new BridgeError(`cannot reach the PC bridge at ${this.base}${hint} (${error?.message ?? error})`)
    }
    const text = await response.text()
    let parsed
    try {
      parsed = text === '' ? undefined : JSON.parse(text)
    } catch {
      parsed = undefined
    }
    if (raw === true) return { status: response.status, text }
    if (!response.ok) {
      const detail = isJsonObject(parsed) && typeof parsed.error === 'string'
        ? parsed.error
        : text.slice(0, 400) || response.statusText
      throw new BridgeError(
        `the PC bridge rejected ${method} ${path}: HTTP ${response.status} — ${detail}`,
        { status: response.status, code: isJsonObject(parsed) ? parsed.code : undefined, body: parsed },
      )
    }
    if (parsed === undefined) {
      throw new BridgeError(`the PC bridge returned a non-JSON body for ${method} ${path}: ${text.slice(0, 400)}`)
    }
    return parsed
  }

  get(path, options) {
    return this.request('GET', path, options)
  }

  post(path, body, options) {
    return this.request('POST', path, { ...options, body })
  }

  health(options) {
    return this.get('/v1/health', options)
  }

  devices(options) {
    return this.get('/v1/devices', options)
  }

  setup(action, options) {
    return this.post('/v1/setup', { action }, options)
  }

  adb(args, { serial, timeoutMs, stdin } = {}) {
    return this.post('/v1/adb', { args, ...(serial ? { serial } : {}), ...(stdin === undefined ? {} : { stdin }) }, { timeoutMs })
  }

  fastboot(args, { serial, timeoutMs } = {}) {
    return this.post('/v1/fastboot', { args, ...(serial ? { serial } : {}) }, { timeoutMs })
  }

  serialOpen(payload, options) {
    return this.post('/v1/serial/open', payload, options)
  }

  serialWrite(payload, options) {
    return this.post('/v1/serial/write', payload, options)
  }

  serialRead(payload, options) {
    return this.post('/v1/serial/read', payload, options)
  }

  serialClose(payload, options) {
    return this.post('/v1/serial/close', payload, options)
  }

  serialPorts(options) {
    return this.get('/v1/serial/ports', options)
  }

  fsPut(payload, options) {
    return this.post('/v1/fs/put', payload, options)
  }

  fsGet(payload, options) {
    return this.post('/v1/fs/get', payload, options)
  }

  fsLs(payload, options) {
    return this.post('/v1/fs/ls', payload, options)
  }
}

/** Flatten a command result into the compact shape tools hand to the model. */
export function summarizeCommand(result, { maxChars = 24_000 } = {}) {
  const clip = value => {
    const text = typeof value === 'string' ? value : ''
    if (text.length <= maxChars) return { text, clipped: false }
    return { text: `${text.slice(text.length - maxChars)}\n…[earlier output clipped]`, clipped: true }
  }
  const stdout = clip(result.stdout)
  const stderr = clip(result.stderr)
  return {
    args: Array.isArray(result.argv) ? result.argv : [],
    exitCode: typeof result.exitCode === 'number' ? result.exitCode : -1,
    stdout: stdout.text,
    stderr: stderr.text,
    ...(stdout.clipped || stderr.clipped ? { outputClipped: true } : {}),
    ...(typeof result.error === 'string' && result.error !== '' ? { error: result.error } : {}),
    durationMs: result.durationMs,
  }
}
