#!/usr/bin/env node
// Drop-in `tsgolint` entrypoint (replaces oxlint-tsgolint via pnpm overrides).
//
// Forwards every invocation to the forked platform binary, a superset of
// upstream tsgolint: `tsgolint headless …` (used by `oxlint --type-aware`) is
// unchanged, and `tsgolint model-codegen` adds the effect-app model type-query.
//
// Binaries are NOT committed. On first use we fetch the current platform's
// gzipped binary from the public release, cache it next to this shim, and
// gunzip it. Only Node is required — no git-lfs, no curl. A locally-built
// `*.gz` (via build.sh) is used if present, so offline/dev builds work too.
"use strict"

const cp = require("node:child_process")
const fs = require("node:fs")
const path = require("node:path")
const zlib = require("node:zlib")

const RELEASE_TAG = "tsgolint-fork-v0.23.0"
const RELEASE_BASE = `https://github.com/effect-app/tsgolint-fork/releases/download/${RELEASE_TAG}`

const exe = process.platform === "win32" ? ".exe" : ""
const name = `tsgolint-codegen-${process.platform}-${process.arch}${exe}`

/** Ensure the platform binary is present (download + extract on first use). */
function ensureBinary() {
  const bin = path.join(__dirname, name)
  if (fs.existsSync(bin)) return bin

  const gz = `${bin}.gz`
  if (!fs.existsSync(gz)) {
    const url = `${RELEASE_BASE}/${name}.gz`
    const r = cp.spawnSync(process.execPath, [path.join(__dirname, "download.js"), url, gz], {
      stdio: ["ignore", "ignore", "inherit"]
    })
    if (r.status !== 0 || !fs.existsSync(gz)) {
      console.error(
        `tsgolint-fork: failed to download the ${process.platform}-${process.arch} binary from ${url}. ` +
          `Check your network, or build locally with repos/tsgolint-fork/build.sh.`
      )
      process.exit(1)
    }
  }
  fs.writeFileSync(bin, zlib.gunzipSync(fs.readFileSync(gz)), { mode: 0o755 })
  return bin
}

module.exports = { ensureBinary, binaryName: name, RELEASE_TAG }

if (require.main === module) {
  const result = cp.spawnSync(ensureBinary(), process.argv.slice(2), { stdio: "inherit" })
  if (result.error) {
    console.error(result.error)
    process.exit(1)
  }
  process.exit(result.status == null ? 1 : result.status)
}
