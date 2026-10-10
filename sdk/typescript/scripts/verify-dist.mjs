// Checks the built package the way a consumer would load it: installs the
// packed tarball into a scratch project and imports it as ESM and through
// require() (Node loads ES modules synchronously for require since 20.19 / 22.12).
import { execFileSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..')
const pkg = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'))
const scratch = mkdtempSync(join(tmpdir(), 'sbx-sdk-verify-'))
const run = (cmd, args, cwd) => execFileSync(cmd, args, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] })

try {
  const packed = JSON.parse(run('npm', ['pack', '--json', '--pack-destination', scratch], root))[0]
  const files = packed.files.map((f) => f.path)
  for (const need of ['dist/index.js', 'dist/index.d.ts', 'README.md', 'LICENSE', 'package.json']) {
    if (!files.includes(need)) throw new Error(`the tarball lacks ${need}`)
  }
  if (files.some((f) => f.startsWith('test/') || f.startsWith('node_modules/'))) throw new Error('the tarball contains test files')

  const app = join(scratch, 'app')
  mkdirSync(app)
  writeFileSync(join(app, 'package.json'), JSON.stringify({ name: 'verify', private: true, type: 'module' }))
  run('npm', ['install', '--no-audit', '--no-fund', '--ignore-scripts', join(scratch, packed.filename)], app)

  const check = `
    const names = ['Sandbox', 'CommandHandle', 'PtyHandle', 'SandboxError', 'CommandExitError', 'RateLimitError', 'VERSION']
    for (const n of names) if (m[n] === undefined) throw new Error('missing export ' + n)
    if (m.VERSION !== ${JSON.stringify(pkg.version)}) throw new Error('version mismatch: ' + m.VERSION)
    if (!(new m.RateLimitError('x') instanceof m.SandboxError)) throw new Error('broken error hierarchy')
    if (typeof m.Sandbox.create !== 'function' || typeof m.Sandbox.prototype[Symbol.asyncDispose] !== 'function') throw new Error('broken Sandbox')
  `
  writeFileSync(
    join(app, 'esm.mjs'),
    `import * as m from '${pkg.name}'\n${check}\nconsole.log('esm import ok:', Object.keys(m).length, 'exports')\n`,
  )
  writeFileSync(
    join(app, 'cjs.cjs'),
    `const m = require('${pkg.name}')\n${check}\nconsole.log('cjs require ok:', Object.keys(m).length, 'exports')\n`,
  )
  // A consumer's type check against the published declarations.
  writeFileSync(
    join(app, 'types.ts'),
    `import { Sandbox, type CommandResult, type SandboxInfo } from '${pkg.name}'\n` +
      `export async function f(): Promise<[CommandResult, SandboxInfo]> {\n` +
      `  const sb = await Sandbox.create('python')\n  return [await sb.commands.run('true'), await sb.getInfo()]\n}\n`,
  )
  process.stdout.write(run('node', ['esm.mjs'], app))
  process.stdout.write(run('node', ['cjs.cjs'], app))
  run(
    join(root, 'node_modules/.bin/tsc'),
    ['--noEmit', '--strict', '--module', 'nodenext', '--target', 'es2022', '--skipLibCheck', 'false', 'types.ts'],
    app,
  )
  console.log('consumer type check ok')

  // The core must not depend on Node: nothing in dist may import a node: module.
  for (const f of files.filter((f) => f.startsWith('dist/') && f.endsWith('.js'))) {
    const src = readFileSync(join(root, f), 'utf8')
    if (/from ['"]node:|require\(['"]node:/.test(src)) throw new Error(`${f} imports a Node module`)
  }
  console.log(`no Node-only imports in dist; tarball has ${files.length} files, ${packed.size} bytes`)
} finally {
  rmSync(scratch, { recursive: true, force: true })
}
