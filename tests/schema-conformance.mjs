#!/usr/bin/env node
/**
 * Conformance of this plugin's tool definitions with the HARNESS's schema gate.
 *
 * `ctx.tools.register()` takes *compiled* raw JSON Schema, and the harness owns
 * the compiler (`defineTool` / `parameterSchemaSpecToJsonSchema` in
 * `@deepseek-ai/dsh-tools`). This plugin is deliberately dependency-free, so it
 * carries a 12-line equivalent (`compileParameterSpec`) — and this test pins the
 * two together instead of trusting them:
 *
 *   1. every registered `parameters` and `output.schema` passes the harness's own
 *      `assertObjectJsonSchema` (the gate that rejected the shipped
 *      `type: null` function schema the API refused with
 *      `Invalid schema for function 'pc_mobile_adb'`);
 *   2. `compileParameterSpec(spec)` is byte-identical to the harness's
 *      `parameterSchemaSpecToJsonSchema(spec)` for representative specs;
 *   3. the harness accepts real arguments and rejects malformed ones.
 *
 * Skips (exit 0) when the harness's package cannot be found, so the plugin's own
 * tests still run outside a Harness checkout.
 *
 * Usage: node tests/schema-conformance.mjs
 */

import { existsSync } from 'node:fs'
import { pathToFileURL } from 'node:url'

import { resolveConfig, BridgeClient } from '../bridge-client.js'
import { compileParameterSpec, createTools } from '../tools.js'

const CANDIDATES = [
  process.env.DSH_TOOLS_PATH,
  '/vol1/@appcenter/Harness/server/node_modules/@deepseek-ai/dsh-tools/lib/index.js',
  '/app/Harness/server/node_modules/@deepseek-ai/dsh-tools/lib/index.js',
].filter(candidate => typeof candidate === 'string' && candidate !== '')

async function loadHarness() {
  for (const candidate of CANDIDATES) {
    if (!existsSync(candidate)) continue
    return await import(pathToFileURL(candidate).href)
  }
  try {
    return await import('@deepseek-ai/dsh-tools')
  } catch {
    return null
  }
}

const harness = await loadHarness()
if (harness === null) {
  console.log('SKIP: @deepseek-ai/dsh-tools not found (set DSH_TOOLS_PATH to run this test)')
  process.exit(0)
}

let failures = 0
const check = (label, ok, detail) => {
  if (ok) {
    console.log(`  PASS ${label}`)
  } else {
    failures += 1
    console.log(`  FAIL ${label}${detail === undefined ? '' : ` (${detail})`}`)
  }
}

const stable = value => JSON.stringify(value, Object.keys(flattenKeys(value)).sort())
function flattenKeys(value, into = {}) {
  if (value !== null && typeof value === 'object') {
    for (const key of Object.keys(value)) {
      into[key] = true
      flattenKeys(value[key], into)
    }
  }
  return into
}

const config = resolveConfig({})
const tools = createTools(new BridgeClient(config), config)

console.log(`\n== harness gate: assertObjectJsonSchema (${tools.length} tools) ==`)
for (const tool of tools) {
  for (const [what, schema] of [['parameters', tool.parameters], ['output.schema', tool.output.schema]]) {
    try {
      harness.assertObjectJsonSchema(schema)
      check(`${tool.name}.${what}`, true)
    } catch (error) {
      check(`${tool.name}.${what}`, false, error.message)
    }
  }
}

console.log('\n== harness gate: args validation ==')
try {
  const adb = tools.find(tool => tool.name === 'pc_mobile_adb')
  const good = harness.validateArgs(
    { args: { type: 'array', items: { type: 'string' }, required: true } },
    { args: ['devices', '-l'] },
  )
  check('well-formed args validate', good.length === 0, JSON.stringify(good))
  const bad = harness.validateArgs(
    { args: { type: 'array', items: { type: 'string' }, required: true } },
    { args: 'devices -l' },
  )
  check('a string where an array belongs is a violation', bad.length > 0, JSON.stringify(bad))
  check('the registered schema is the compiled one', adb.parameters.type === 'object')
} catch (error) {
  check('args validation', false, error.message)
}

console.log('\n== compiler faithfulness vs parameterSchemaSpecToJsonSchema ==')
const SPECS = {
  'empty parameters': {},
  'required array of strings + optional scalar':
    { args: { type: 'array', items: { type: 'string' }, required: true }, serial: { type: 'string' } },
  'every scalar kind': {
    flag: { type: 'boolean' },
    count: { type: 'integer' },
    ratio: { type: 'number' },
    name: { type: 'string' },
    nothing: { type: 'null' },
  },
  'enum + const': { op: { type: 'string', enum: ['a', 'b'], required: true } },
  'nested object with additionalProperties': {
    filter: { type: 'object', additionalProperties: true, required: true },
  },
  'array of objects': {
    rows: {
      type: 'array',
      required: true,
      items: { type: 'object', additionalProperties: false, properties: { id: { type: 'string' } } },
    },
  },
}
for (const [label, spec] of Object.entries(SPECS)) {
  const mine = compileParameterSpec(spec)
  let theirs
  try {
    theirs = harness.parameterSchemaSpecToJsonSchema(spec)
  } catch (error) {
    check(`${label} (harness compiled it)`, false, error.message)
    continue
  }
  const same = stable(mine) === stable(theirs)
  check(label, same, same ? undefined : `mine=${JSON.stringify(mine)} theirs=${JSON.stringify(theirs)}`)
}

console.log(`\n${failures === 0 ? 'ALL CHECKS PASSED' : `${failures} failure(s)`}`)
process.exit(failures === 0 ? 0 : 1)
