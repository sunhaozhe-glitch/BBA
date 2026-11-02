#!/usr/bin/env node
/*
 * Minimal pinned patch for koishi-plugin-chatluna 1.4.4.
 *
 * Upstream `ChatLunaService.uninstallPlugin()` logs through
 * `this.ctx.logger.success(...)`. Cordis disposes the logger service before
 * plugin dispose callbacks run, so at App teardown `this.ctx.logger` is already
 * undefined and the log call throws
 *   TypeError: Cannot read properties of undefined (reading 'success').
 * The plugin record has already been removed at that point; only the teardown
 * log fails. The same dispose closure also calls `ctx.chatluna.uninstallPlugin`
 * after the service may be gone, which would raise a second teardown TypeError.
 *
 * This patch makes both teardown accesses optional. It never suppresses a
 * runtime error outside the exact upstream 1.4.4 patterns: a version or source
 * change fails the patch instead of silently doing nothing.
 *
 * Usage: node tools/patch-chatluna.cjs [package-root]
 * The optional package root is the package whose node_modules must be patched
 * (character-runtime by default; the collector passes its own root).
 */
'use strict'

const { createRequire } = require('node:module')
const { readFile, writeFile, stat } = require('node:fs/promises')
const path = require('node:path')

const EXPECTED_VERSION = '1.4.4'
const packageRoot = path.resolve(process.argv[2] || path.join(__dirname, '..'))
const requireFromPackage = createRequire(path.join(packageRoot, 'package.json'))

async function main() {
  let entry
  try {
    entry = requireFromPackage.resolve('koishi-plugin-chatluna')
  } catch (error) {
    console.error(`chatluna-patch: cannot resolve koishi-plugin-chatluna from ${packageRoot}: ${error.message}`)
    process.exit(1)
  }
  const pluginRoot = path.resolve(path.dirname(entry), '..')
  const manifestPath = path.join(pluginRoot, 'package.json')
  const manifest = JSON.parse(await readFile(manifestPath, 'utf8'))
  if (manifest.name !== 'koishi-plugin-chatluna' || manifest.version !== EXPECTED_VERSION) {
    console.error(
      `chatluna-patch: expected koishi-plugin-chatluna@${EXPECTED_VERSION}, found ${manifest.name}@${manifest.version}`
    )
    process.exit(1)
  }

  let changed = 0
  for (const relative of ['lib/services/chat.cjs', 'lib/services/chat.mjs']) {
    const file = path.join(pluginRoot, relative)
    let source
    try {
      source = await readFile(file, 'utf8')
    } catch (error) {
      if (error.code === 'ENOENT') continue
      throw error
    }
    const before = source
    source = source.replace(
      /this\.ctx\.logger\.success\(\s*"Plugin %c uninstalled",\s*targetPlugin\.platformName\s*\);/g,
      'this.ctx.logger?.success?.(\n      "Plugin %c uninstalled",\n      targetPlugin.platformName\n    );'
    )
    source = source.replace(
      /ctx\.chatluna\.uninstallPlugin\(this\);/g,
      'ctx.chatluna?.uninstallPlugin?.(this);'
    )
    source = source.replace(
      /this\.ctx\.chatluna\.uninstallPlugin\(this\);/g,
      'this.ctx.chatluna?.uninstallPlugin?.(this);'
    )
    // An aborted ChatLuna invocation runs cleanup in a finally block. Cordis
    // can dispose the database before that finally executes, which used to
    // throw `Cannot read properties of undefined (reading 'remove')` from the
    // plugin dispose path. The cleanup is already best-effort: skip it only
    // when the database service is gone, and keep its warnings optional.
    source = source.replace(
      /(async cleanupTransient\(conversation\) \{\n    if \(conversation == null\) return;\n)(?:    if \(!this\.service\?\.ctx\?\.database\) return;\n)+/g,
      '$1'
    )
    source = source.replace(
      /(async cleanupTransient\(conversation\) \{\n    if \(conversation == null\) return;\n)/g,
      '$1    if (!this.service?.ctx?.database) return;\n'
    )
    source = source.replace(
      /this\.service\.ctx\.logger\.warn\(\n        "Failed to (remove transient|clear cache)/g,
      'this.service.ctx.logger?.warn?.(\n        "Failed to $1'
    )
    if (source !== before) {
      const { mode } = await stat(file)
      await writeFile(file, source, { encoding: 'utf8', mode })
      changed += 1
      console.log(`chatluna-patch: patched ${relative} (${manifest.version})`)
    } else if (
      source.includes('this.ctx.logger?.success?.(') &&
      source.includes('ctx.chatluna?.uninstallPlugin?.(this);') &&
      source.includes('this.ctx.chatluna?.uninstallPlugin?.(this);') &&
      source.includes('if (!this.service?.ctx?.database) return;')
    ) {
      console.log(`chatluna-patch: ${relative} already patched (${manifest.version})`)
    } else {
      console.error(`chatluna-patch: pinned source pattern not found in ${relative}; refusing to continue`)
      process.exit(1)
    }
  }
  if (changed === 0) {
    console.log(`chatluna-patch: ${manifest.version} teardown guards verified`)
  }
}

main().catch((error) => {
  console.error(`chatluna-patch: ${error.stack || error.message}`)
  process.exit(1)
})
