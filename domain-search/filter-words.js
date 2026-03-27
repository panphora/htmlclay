import fs from 'node:fs/promises'

const AFINN_FILE = '/tmp/afinn-165.txt'
const PROFANITY_FILE = '/tmp/profanity.txt'
const WORD_FILES = ['3-letter.txt', '4-letter.txt', '5-letter.txt']
const AFINN_THRESHOLD = -3

async function loadAfinn(path) {
  const raw = await fs.readFile(path, 'utf8')
  const map = new Map()
  for (const line of raw.split('\n')) {
    if (!line.trim()) continue
    const parts = line.split('\t')
    const word = parts[0].trim().toLowerCase()
    const score = parseInt(parts[1], 10)
    if (word && !isNaN(score)) map.set(word, score)
  }
  return map
}

async function loadProfanity(path) {
  const raw = await fs.readFile(path, 'utf8')
  const set = new Set()
  for (const line of raw.split('\n')) {
    const word = line.trim().toLowerCase()
    if (word && !word.includes(' ')) set.add(word)
  }
  return set
}

async function main() {
  const afinn = await loadAfinn(AFINN_FILE)
  const profanity = await loadProfanity(PROFANITY_FILE)

  let allWords = []
  for (const file of WORD_FILES) {
    const raw = await fs.readFile(file, 'utf8')
    const words = raw.split('\n').map(w => w.trim().toLowerCase()).filter(Boolean)
    allWords.push(...words)
  }

  const cut = []
  const kept = []

  for (const word of allWords) {
    const afinnScore = afinn.get(word)
    const isProfane = profanity.has(word)
    const isBadSentiment = afinnScore !== undefined && afinnScore <= AFINN_THRESHOLD

    if (isProfane || isBadSentiment) {
      const reasons = []
      if (isProfane) reasons.push('profanity')
      if (isBadSentiment) reasons.push(`afinn=${afinnScore}`)
      cut.push({ word, reasons: reasons.join(', ') })
    } else {
      kept.push(word)
    }
  }

  cut.sort((a, b) => a.word.localeCompare(b.word))
  kept.sort((a, b) => a.localeCompare(b))

  const cutReport = cut.map(c => `${c.word.padEnd(20)} ${c.reasons}`).join('\n')
  await fs.writeFile('cut-words.txt', cutReport, 'utf8')

  const domains = kept.map(w => `${w}clay.com`)
  await fs.writeFile('domains.txt', domains.join('\n') + '\n', 'utf8')

  console.log(`Total words: ${allWords.length}`)
  console.log(`Cut: ${cut.length}`)
  console.log(`Kept: ${kept.length}`)
  console.log(`\nCut words written to: cut-words.txt`)
  console.log(`Domains written to: domains.txt (${domains.length} domains)`)
  console.log(`\n--- CUT LIST ---\n`)
  console.log(cutReport)
}

main().catch(err => {
  console.error(err)
  process.exit(1)
})
