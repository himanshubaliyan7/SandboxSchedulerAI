/**
 * An interactive shell in the sandbox, in your own terminal.
 *
 *   export SBX_API_KEY=sbx_...  SBX_BASE_URL=http://127.0.0.1:8080
 *   npm run build && node examples/02-interactive-pty.ts
 *
 * Type `exit` (or press Ctrl+D) to leave. Ctrl+C goes to the sandbox.
 */
import { Sandbox } from 'sbx-sdk'

if (!process.stdin.isTTY || !process.stdout.isTTY) {
  console.error('this example needs a terminal')
  process.exit(1)
}

await using sandbox = await Sandbox.create(process.env.SBX_TEMPLATE ?? 'python')

const pty = await sandbox.pty.create({
  cols: process.stdout.columns,
  rows: process.stdout.rows,
  // Resolving only when the terminal has taken the bytes is what lets the
  // SDK's flow control slow down a command that prints faster than we draw.
  onData: (data) =>
    new Promise<void>((resolve) => {
      if (process.stdout.write(data)) resolve()
      else process.stdout.once('drain', resolve)
    }),
})

// Raw mode: every key, including Ctrl+C, goes to the sandbox unprocessed.
process.stdin.setRawMode(true)
process.stdin.resume()
const onInput = (chunk: Buffer): void => pty.sendInput(chunk)
const onResize = (): void => {
  if (process.stdout.columns > 0 && process.stdout.rows > 0) pty.resize({ cols: process.stdout.columns, rows: process.stdout.rows })
}
process.stdin.on('data', onInput)
process.on('SIGWINCH', onResize)

try {
  const { exitCode } = await pty.wait()
  process.exitCode = exitCode
} finally {
  process.off('SIGWINCH', onResize)
  process.stdin.off('data', onInput)
  process.stdin.setRawMode(false)
  process.stdin.pause()
}
