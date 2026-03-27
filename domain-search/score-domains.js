import fs from 'node:fs/promises'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'

const exec = promisify(execFile)

const INPUT_FILE = 'available.txt'
const OUTPUT_FILE = 'scored-domains.json'
const BATCH_SIZE = 100
const CONCURRENCY = 10
const CLAUDE_PATH = process.env.HOME + '/.local/bin/claude'

const PROJECT_CONTEXT = `Malleable is a desktop app for self-saving HTML files. Users double-click a .malleable file, edit it in a browser, and changes persist to disk. No cloud, no accounts. Key values: portability, simplicity, creativity, local-first, developer-friendly, approachable.`

const SYSTEM_PROMPT = `You score domain names for a product called Malleable. ${PROJECT_CONTEXT}

Each domain is {word}clay.com. Score ONLY the word's fit as a brand for this product.

Respond with ONLY a JSON array. Each entry: {"domain":"...","score":N,"reason":"..."}
Score 1-10: 1=terrible fit, 5=neutral, 10=perfect fit.
Reason must be under 10 words.
No markdown, no explanation, ONLY the JSON array.`

function chunk(array, size) {
  const out = []
  for (let i = 0; i < array.length; i += size) {
    out.push(array.slice(i, i + size))
  }
  return out
}

async function scoreBatch(domains) {
  const prompt = `Score these domains:\n${domains.join('\n')}`

  const { stdout } = await exec(CLAUDE_PATH, [
    '-p', prompt,
    '--model', 'haiku',
    '--system-prompt', SYSTEM_PROMPT,
    '--output-format', 'text',
    '--no-session-persistence',
  ], { maxBuffer: 1024 * 1024, timeout: 120000 })

  const text = stdout.trim()
  const jsonMatch = text.match(/\[[\s\S]*\]/)
  if (!jsonMatch) throw new Error(`No JSON array in response: ${text.slice(0, 200)}`)

  return JSON.parse(jsonMatch[0])
}

async function main() {
  const raw = await fs.readFile(INPUT_FILE, 'utf8')
  const domains = raw.split(/\r?\n/).map(s => s.trim()).filter(Boolean)

  const batches = chunk(domains, BATCH_SIZE)
  const totalBatches = batches.length

  console.log(`Domains: ${domains.length} | Batches: ${totalBatches} | Concurrency: ${CONCURRENCY}`)
  console.log()

  const results = new Array(totalBatches)
  let completed = 0
  let failures = 0

  const waves = chunk(batches, CONCURRENCY)

  for (let w = 0; w < waves.length; w++) {
    const wave = waves[w]
    const waveOffset = w * CONCURRENCY

    const promises = wave.map(async (batch, j) => {
      const idx = waveOffset + j
      try {
        const scored = await scoreBatch(batch)
        results[idx] = scored
        completed++
        const avg = (scored.reduce((s, d) => s + d.score, 0) / scored.length).toFixed(1)
        console.log(`  [${completed}/${totalBatches}] Batch ${idx + 1} — ${scored.length} scored, avg ${avg}`)
      } catch (err) {
        failures++
        completed++
        console.error(`  [${completed}/${totalBatches}] Batch ${idx + 1} FAILED: ${err.message.slice(0, 100)}`)
        results[idx] = batch.map(d => ({ domain: d, score: -1, reason: 'scoring failed' }))
      }
    })

    await Promise.all(promises)
    console.log(`Wave ${w + 1}/${waves.length} done`)
  }

  const allScored = results.flat()
  allScored.sort((a, b) => b.score - a.score)

  await fs.writeFile(OUTPUT_FILE, JSON.stringify(allScored, null, 2), 'utf8')

  const top = allScored.filter(d => d.score >= 8)
  const good = allScored.filter(d => d.score >= 6 && d.score < 8)
  const mid = allScored.filter(d => d.score >= 4 && d.score < 6)
  const low = allScored.filter(d => d.score < 4 && d.score >= 0)

  await fs.writeFile('top-domains.txt',
    top.map(d => `${d.domain.padEnd(25)} ${d.score}/10  ${d.reason}`).join('\n') + '\n', 'utf8')

  console.log()
  console.log(`Done. Full results: ${OUTPUT_FILE}`)
  console.log(`Top picks (8+): ${top.length} → top-domains.txt`)
  console.log(`Good (6-7): ${good.length}`)
  console.log(`Neutral (4-5): ${mid.length}`)
  console.log(`Low (1-3): ${low.length}`)
  console.log(`Failures: ${failures} batches`)
}

main().catch(err => {
  console.error(err)
  process.exit(1)
})
