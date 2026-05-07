#!/usr/bin/env node
// Build release-manifest.json from per-archive metadata files.
//
// The per-archive metadata is emitted by the build job in release.yml as
// dist/meta/<name>.<ext>.json. This script aggregates them into a single
// release-manifest.json that gets uploaded to the GitHub release alongside
// SHA256SUMS. The cloud manifest endpoint synthesizes URLs separately;
// this manifest gives integrity-conscious clients (or a future cloud v2)
// a verified payload — sha256 + size + build_time + minimum_server_version.
//
// Usage:
//   node build-manifest.mjs --version <tag> --repo <owner/repo> \
//     --meta-dir <dir> --out <path> [--server-min <semver>]

import { readFileSync, readdirSync, writeFileSync } from "node:fs"
import { join } from "node:path"

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
const required = ["version", "repo", "meta-dir", "out"]
for (const k of required) {
  if (!args[k]) {
    console.error(`missing required --${k}`)
    process.exit(2)
  }
}

const version = String(args.version)
const repo = String(args.repo) // owner/name
const metaDir = String(args["meta-dir"])
const outPath = String(args.out)
const serverMin = args["server-min"] ? String(args["server-min"]) : "0.0.0"
const downloadBase = `https://github.com/${repo}/releases/download/${version}`
const buildTime = new Date().toISOString()

const metaFiles = readdirSync(metaDir).filter((f) => f.endsWith(".json"))
if (metaFiles.length !== 6) {
  console.error(
    `expected 6 metadata files in ${metaDir}, got ${metaFiles.length}`,
  )
  process.exit(1)
}

const artifacts = metaFiles
  .map((f) => JSON.parse(readFileSync(join(metaDir, f), "utf8")))
  .map((m) => ({
    name: m.name,
    os: m.os,
    arch: m.arch,
    kind: m.kind,
    ext: m.ext,
    sha256: m.sha256,
    size_bytes: m.size_bytes,
    download_url: `${downloadBase}/${m.name}`,
    build_time: buildTime,
    minimum_server_version: serverMin,
  }))
  .sort((a, b) =>
    `${a.os}_${a.arch}` < `${b.os}_${b.arch}` ? -1 : 1,
  )

const manifest = {
  schema_version: 1,
  version,
  repo,
  released_at: buildTime,
  artifacts,
}

writeFileSync(outPath, JSON.stringify(manifest, null, 2) + "\n")
console.log(`wrote ${outPath} (${artifacts.length} artifacts)`)
