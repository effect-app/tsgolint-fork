#!/usr/bin/env node
// Minimal redirect-following downloader (GitHub release assets redirect to a
// CDN). Run synchronously by the shim via spawnSync; Node-only, no deps.
//   node download.js <url> <dest>
"use strict"

const fs = require("node:fs")
const https = require("node:https")

const [, , url, dest] = process.argv

function get(u, redirects) {
  https
    .get(u, { headers: { "user-agent": "tsgolint-fork" } }, (res) => {
      const code = res.statusCode ?? 0
      if ([301, 302, 303, 307, 308].includes(code) && res.headers.location) {
        res.resume()
        if (redirects <= 0) return fail(new Error("too many redirects"))
        return get(res.headers.location, redirects - 1)
      }
      if (code !== 200) {
        res.resume()
        return fail(new Error(`HTTP ${code}`))
      }
      const tmp = `${dest}.download`
      const file = fs.createWriteStream(tmp)
      res.pipe(file)
      file.on("finish", () => file.close(() => {
        fs.renameSync(tmp, dest)
        process.exit(0)
      }))
      file.on("error", fail)
    })
    .on("error", fail)
}

function fail(err) {
  console.error(String(err && err.message ? err.message : err))
  process.exit(1)
}

if (!url || !dest) fail(new Error("usage: download.js <url> <dest>"))
get(url, 5)
