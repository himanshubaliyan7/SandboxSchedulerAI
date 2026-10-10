/**
 * Run LLM-style Python code in a sandbox and print the result.
 *
 *   export SBX_API_KEY=sbx_...  SBX_BASE_URL=http://127.0.0.1:8080
 *   npm run build && node examples/01-run-code.ts
 */
import { CommandExitError, Sandbox } from 'sbx-sdk'

// What a model might hand back when asked "what is the 30th Fibonacci number?".
const code = `
def fib(n):
    a, b = 0, 1
    for _ in range(n):
        a, b = b, a + b
    return a

print(fib(30))
`

// `await using` kills the sandbox when this scope ends, also on an error.
await using sandbox = await Sandbox.create(process.env.SBX_TEMPLATE ?? 'python', { timeoutMs: 60_000 })
console.log(`sandbox ${sandbox.sandboxId}`)

try {
  // The program goes in on stdin, so no quoting is involved.
  const result = await sandbox.commands.run('python3', {
    args: ['-'],
    stdin: code,
    timeoutMs: 10_000,
    onStderr: (chunk) => process.stderr.write(chunk),
  })
  console.log(`result: ${result.stdout.trim()}`)
} catch (e) {
  if (!(e instanceof CommandExitError)) throw e
  // The model's code failed: this is what to show it for another attempt.
  console.error(`the code failed (${e.reason}, exit code ${e.exitCode}):\n${e.stderr}`)
  process.exitCode = 1
}
