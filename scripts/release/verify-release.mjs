#!/usr/bin/env node
// Smoke-validate a published release: confirm all expected assets exist
// and resolve to a 200 (or a redirect-chain ending in 200). Fails hard
// when anything is missing or unreachable so the release job fails loudly
// rather than producing a half-published release.
//
// The expected asset set is:
//   - 6 archives matching astery-engine-tools_<version>_<os>_<arch>.<ext>
//   - SHA256SUMS
//   - release-manifest.json
//
// Usage:
//   node verify-release.mjs --tag <version> --repo <owner/repo>
//   node verify-release.mjs --dry-run --fixture <path>
//
// Dry-run mode reads a fixture release JSON (matching `gh api` output) so
// the script can be unit-tested locally without hitting the network.

import { readFileSync } from "node:fs"
import { execFileSync } from "node:child_process"

function parseArgs(argv) {
  const out = {}
  for (let i = 2; i < argv.length; i++) {
    const k = argv[i]
    if (!k.startsWith("--")) continue
    const v = argv[i + 1]
    if (v === undefined || v.startsWith("--")) {
      out[k.slice(2)] = true
    } else {
      out[k.slice(2)] = v
      i++
    }
  }
  return out
}

const args = parseArgs(process.argv)

function expectedAssetNames(version) {
  const matrix = [
    { os: "darwin", arch: "x64", ext: "tar.gz" },
    { os: "darwin", arch: "arm64", ext: "tar.gz" },
    { os: "windows", arch: "x64", ext: "zip" },
    { os: "windows", arch: "arm64", ext: "zip" },
    { os: "linux", arch: "x64", ext: "tar.gz" },
    { os: "linux", arch: "arm64", ext: "tar.gz" },
  ]
  const archives = matrix.map(
    (m) => `astery-engine-tools_${version}_${m.os}_${m.arch}.${m.ext}`,
  )
  return [...archives, "SHA256SUMS", "release-manifest.json"]
}

async function fetchRelease(repo, tag) {
  const raw = execFileSync(
    "gh",
    ["api", `repos/${repo}/releases/tags/${tag}`],
    { encoding: "utf8" },
  )
  return JSON.parse(raw)
}

async function head(url) {
  const res = await fetch(url, { method: "HEAD", redirect: "follow" })
  return res.status
}

function loadFixture(path) {
  return JSON.parse(readFileSync(path, "utf8"))
}

async function main() {
  if (args["dry-run"]) {
    if (!args.fixture) {
      console.error("--dry-run requires --fixture <path>")
      process.exit(2)
    }
    const fixture = loadFixture(String(args.fixture))
    const tag = fixture.tag_name
    const expected = expectedAssetNames(tag)
    const actual = (fixture.assets || []).map((a) => a.name)
    const missing = expected.filter((n) => !actual.includes(n))
    if (missing.length) {
      console.error(`fixture missing assets: ${missing.join(", ")}`)
      process.exit(1)
    }
    console.log(`dry-run OK: ${expected.length} expected assets present`)
    return
  }

  if (!args.tag || !args.repo) {
    console.error("usage: --tag <version> --repo <owner/repo>")
    process.exit(2)
  }
  const tag = String(args.tag)
  const repo = String(args.repo)
  const expected = expectedAssetNames(tag)

  const release = await fetchRelease(repo, tag)
  const byName = new Map((release.assets || []).map((a) => [a.name, a]))

  const missing = expected.filter((n) => !byName.has(n))
  if (missing.length) {
    console.error(`missing assets: ${missing.join(", ")}`)
    process.exit(1)
  }

  let failures = 0
  for (const name of expected) {
    const asset = byName.get(name)
    const url = asset.browser_download_url
    const status = await head(url)
    if (status >= 200 && status < 400) {
      console.log(`ok   ${status}  ${name}`)
    } else {
      console.error(`fail ${status}  ${name}  (${url})`)
      failures++
    }
  }
  if (failures > 0) {
    console.error(`smoke validation failed: ${failures} unreachable assets`)
    process.exit(1)
  }
  console.log(`smoke validation OK: ${expected.length} assets reachable`)
}

await main()
