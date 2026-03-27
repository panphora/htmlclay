import fs from 'node:fs/promises'
import { XMLParser } from 'fast-xml-parser'
import 'dotenv/config'

const API_USER = process.env.NAMECHEAP_API_USER
const API_KEY = process.env.NAMECHEAP_API_KEY
const USER_NAME = process.env.NAMECHEAP_USER || API_USER
const CLIENT_IP = process.env.NAMECHEAP_CLIENT_IP

const INPUT_FILE = 'domains.txt'
const OUTPUT_FILE = 'results.csv'
const BATCH_SIZE = 50
const REQUEST_DELAY_MS = 1300

const USE_SANDBOX = process.argv.includes('--sandbox')
const TEST_MODE = process.argv.includes('--test')

const ENDPOINT = USE_SANDBOX
  ? 'https://api.sandbox.namecheap.com/xml.response'
  : 'https://api.namecheap.com/xml.response'

if (!API_USER || !API_KEY || !CLIENT_IP) {
  console.error('Missing env vars. Set NAMECHEAP_API_USER, NAMECHEAP_API_KEY, NAMECHEAP_CLIENT_IP')
  process.exit(1)
}

const parser = new XMLParser({
  ignoreAttributes: false,
  attributeNamePrefix: ''
})

function chunk(array, size) {
  const out = []
  for (let i = 0; i < array.length; i += size) {
    out.push(array.slice(i, i + size))
  }
  return out
}

function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, ms))
}

async function main() {
  const raw = await fs.readFile(INPUT_FILE, 'utf8')
  let domains = raw.split(/\r?\n/).map(s => s.trim().toLowerCase()).filter(Boolean)

  if (TEST_MODE) {
    domains = domains.slice(0, 3)
    console.log(`TEST MODE: checking only ${domains.length} domains`)
  }

  console.log(`Endpoint: ${ENDPOINT}`)
  console.log(`Domains to check: ${domains.length}`)
  console.log(`Batches: ${Math.ceil(domains.length / BATCH_SIZE)}`)
  console.log()

  const batches = chunk(domains, BATCH_SIZE)
  const rows = [['domain', 'available', 'isPremiumName', 'premiumRegistrationPrice', 'premiumRenewalPrice', 'icannFee', 'eapFee']]

  for (let i = 0; i < batches.length; i++) {
    const batch = batches[i]

    const params = new URLSearchParams({
      ApiUser: API_USER,
      ApiKey: API_KEY,
      UserName: USER_NAME,
      Command: 'namecheap.domains.check',
      ClientIp: CLIENT_IP,
      DomainList: batch.join(',')
    })

    const url = `${ENDPOINT}?${params.toString()}`
    const res = await fetch(url)

    if (!res.ok) {
      console.error(`HTTP ${res.status} on batch ${i + 1}`)
      const body = await res.text()
      console.error(body.slice(0, 500))
      process.exit(1)
    }

    const xml = await res.text()
    const json = parser.parse(xml)

    const apiResponse = json.ApiResponse
    if (apiResponse.Status !== 'OK') {
      const errors = apiResponse.Errors?.Error
      console.error(`API error on batch ${i + 1}:`, JSON.stringify(errors, null, 2))
      process.exit(1)
    }

    let results = apiResponse.CommandResponse?.DomainCheckResult ?? []
    if (!Array.isArray(results)) results = [results]

    for (const item of results) {
      rows.push([
        item.Domain ?? '',
        item.Available ?? '',
        item.IsPremiumName ?? '',
        item.PremiumRegistrationPrice ?? '',
        item.PremiumRenewalPrice ?? '',
        item.IcannFee ?? '',
        item.EapFee ?? ''
      ])
    }

    const batchAvailable = results.filter(r => String(r.Available) === 'true').length
    console.log(`Batch ${i + 1}/${batches.length} — ${batchAvailable}/${batch.length} available`)

    if (i < batches.length - 1) await sleep(REQUEST_DELAY_MS)
  }

  const csv = rows
    .map(row => row.map(value => `"${String(value).replaceAll('"', '""')}"`).join(','))
    .join('\n')

  await fs.writeFile(OUTPUT_FILE, csv, 'utf8')

  const dataRows = rows.slice(1)
  const available = dataRows.filter(r => String(r[1]) === 'true' && String(r[2]) !== 'true')
  const availablePremium = dataRows.filter(r => String(r[1]) === 'true' && String(r[2]) === 'true')

  await fs.writeFile('available.txt', available.map(r => r[0]).join('\n') + '\n', 'utf8')
  await fs.writeFile('available-premium.txt', availablePremium.map(r => r[0]).join('\n') + '\n', 'utf8')

  console.log()
  console.log(`Done. Results: ${OUTPUT_FILE}`)
  console.log(`Available (non-premium): ${available.length} → available.txt`)
  console.log(`Available (premium): ${availablePremium.length} → available-premium.txt`)
  console.log(`Unavailable: ${dataRows.length - available.length - availablePremium.length}`)
}

main().catch(err => {
  console.error(err)
  process.exit(1)
})
