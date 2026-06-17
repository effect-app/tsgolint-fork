#!/usr/bin/env node
// Drop-in `tsgolint` entrypoint (replaces oxlint-tsgolint via pnpm overrides).
//
// Forwards every invocation to the forked platform binary, which is a superset
// of upstream tsgolint: `tsgolint headless ...` (used by `oxlint --type-aware`)
// behaves identically to upstream, and `tsgolint model-codegen` adds the
// effect-app model type-query command.
//
// Binaries are committed gzipped (one per platform); we lazily gunzip the one
// for the current platform next to the archive on first run.
"use strict"

const cp = require("node:child_process")
const fs = require("node:fs")
const path = require("node:path")
const zlib = require("node:zlib")

const exe = process.platform === "win32" ? ".exe" : ""
const name = `tsgolint-codegen-${process.platform}-${process.arch}${exe}`

/** Ensure the platform binary is extracted from its `.gz` and return its path. */
function ensureBinary() {
  const bin = path.join(__dirname, name)
  if (fs.existsSync(bin)) return bin
  const gz = `${bin}.gz`
  if (!fs.existsSync(gz)) {
    console.error(
      `tsgolint-fork: no prebuilt binary for ${process.platform}-${process.arch} ` +
        `(missing ${name}.gz). Build one with repos/tsgolint-fork/build.sh.`
    )
    process.exit(1)
  }
  fs.writeFileSync(bin, zlib.gunzipSync(fs.readFileSync(gz)), { mode: 0o755 })
  return bin
}

module.exports = { ensureBinary, binaryName: name }

if (require.main === module) {
  const result = cp.spawnSync(ensureBinary(), process.argv.slice(2), { stdio: "inherit" })
  if (result.error) {
    console.error(result.error)
    process.exit(1)
  }
  process.exit(result.status == null ? 1 : result.status)
}
