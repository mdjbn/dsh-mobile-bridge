/**
 * dsh-pc-mobile-bridge — control a phone that is plugged into a WINDOWS PC from
 * a DSH server that has no USB access to it.
 *
 * The plugin itself is a thin, dependency-free host plugin: it holds no adb and
 * no device state, it only speaks HTTP to `dsh-mobile-bridge.exe`, the helper
 * the user runs on the PC. That split is deliberate — this server is a NAS with
 * no adb and no USB, while the PC is the machine that can actually see the
 * phone (over USB, over fastboot, and as a COM port in download mode).
 *
 * Configuration comes from this plugin's row in the profile patch:
 *
 *     - id: pc-mobile-bridge
 *       name: "dsh-pc-mobile-bridge"
 *       config:
 *         bridgeUrl: "http://192.168.1.20:8787"
 *         token: "<token dsh-mobile-bridge.exe printed on startup>"
 *
 * `DSH_PC_MOBILE_BRIDGE_URL` and `DSH_PC_MOBILE_BRIDGE_TOKEN` are read as a
 * fallback. With neither set the plugin still mounts, and every tool answers
 * with instructions instead of a transport error.
 *
 * @module dsh-pc-mobile-bridge
 */

import { BridgeClient, UNCONFIGURED_MESSAGE, resolveConfig } from './bridge-client.js'
import { createTools } from './tools.js'

export const name = 'dsh-pc-mobile-bridge'

export const inject = ['tools']

export function apply(ctx, rawConfig = {}) {
  const config = resolveConfig(rawConfig)
  const client = new BridgeClient(config)
  const tools = createTools(client, config)
  const disposers = []

  for (const tool of tools) {
    disposers.push(ctx.effect(() => ctx.tools.register(tool), `dsh-pc-mobile-bridge:${tool.name}`))
  }

  if (client.configured) {
    ctx.logger.info(`dsh-pc-mobile-bridge mounted (${tools.length} tools → ${config.bridgeUrl}; adb server port `
      + `${config.adbServerPort})`)
  } else {
    ctx.logger.warn(`dsh-pc-mobile-bridge mounted (${tools.length} tools) but is NOT configured — `
      + 'every pc_mobile_* call will explain how to fix it')
    ctx.logger.warn(UNCONFIGURED_MESSAGE)
  }

  return () => {
    for (const dispose of disposers.reverse()) dispose()
  }
}
