/**
 * Upload a file, process it in the sandbox, download the output.
 *
 *   export SBX_API_KEY=sbx_...  SBX_BASE_URL=http://127.0.0.1:8080
 *   npm run build && node examples/03-files.ts
 */
import { Sandbox } from 'sbx-sdk'

const csv = ['city,temp_c', 'Oslo,4.5', 'Cairo,31.0', 'Lima,19.5', 'Oslo,6.5', 'Cairo,29.0'].join('\n')

const script = `
import csv, json, collections
temps = collections.defaultdict(list)
with open("input/temps.csv") as f:
    for row in csv.DictReader(f):
        temps[row["city"]].append(float(row["temp_c"]))
report = {city: round(sum(v) / len(v), 2) for city, v in sorted(temps.items())}
with open("output/report.json", "w") as f:
    json.dump(report, f, indent=2)
print(f"{len(report)} cities")
`

await using sandbox = await Sandbox.create(process.env.SBX_TEMPLATE ?? 'python')

// Paths are relative to the work directory, /workspace; parents are created.
await sandbox.files.write('input/temps.csv', csv)
await sandbox.files.write('process.py', script)
await sandbox.files.makeDir('output')

const run = await sandbox.commands.run('python3', { args: ['process.py'] })
console.log(run.stdout.trim())

for (const entry of await sandbox.files.list('output')) {
  console.log(`${entry.type}  ${entry.size} bytes  ${entry.path}`)
}

const report = JSON.parse(await sandbox.files.read('output/report.json')) as Record<string, number>
console.log(report)

// Large results can be streamed instead of held in memory:
const stream = await sandbox.files.read('output/report.json', { format: 'stream' })
let bytes = 0
for await (const chunk of stream) bytes += chunk.byteLength
console.log(`streamed ${bytes} bytes`)
