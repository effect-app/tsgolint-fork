# tsgolint-fork — drop-in oxlint-tsgolint replacement

A fork of [`tsgolint`](https://github.com/oxc-project/tsgolint) that is a strict
**superset** of upstream:

- `tsgolint headless …` — unchanged upstream behaviour; this is what
  `oxlint --type-aware` spawns.
- `tsgolint model-codegen` — added subcommand that expands an effect-app model
  schema's `Encoded`/`Type`/`Make`/services/facade members on
  [`typescript-go`](https://github.com/microsoft/typescript-go)'s checker
  (native counterpart of `eslint-codegen-model`'s classic `typescript` resolver).

## Drop-in wiring

The repo root `package.json` overrides the dependency:

```jsonc
"pnpm": { "overrides": { "oxlint-tsgolint": "link:./repos/tsgolint-fork" } }
```

So after `pnpm i`, `node_modules/.bin/tsgolint` is this fork. Everyone on the
project automatically gets it — `oxlint --type-aware` runs the fork, and
`eslint-codegen-model`'s native resolver (`createNativeModelTypeResolver`)
resolves the **same** binary via `oxlint-tsgolint/bin/tsgolint.js`. One binary,
both roles. No publish step.

## Binaries

Prebuilt per-platform binaries are committed **gzipped** (`bin/*.gz`, ~13 MB
each). `bin/tsgolint.js` gunzips the current platform's binary next to the
archive on first run (the extracted file is gitignored). Platforms:
linux/darwin/win32 × x64/arm64.

## Rebuilding

```sh
./build.sh --all     # cross-compiles the matrix, writes bin/*.gz
```

Requires a Go toolchain. Pinned to tsgolint `d4d312e` / typescript-go `ccc17db`
(see `build.sh`). The fork delta is a single Go file, `cmd/model_codegen.go`;
`build.sh` clones upstream at the pin, applies its patches, drops the file in,
and registers the `model-codegen` dispatch.

## Protocol (`model-codegen`)

One JSON request on stdin, one JSON response on stdout:

```jsonc
// request
{ "tsconfig": "api/src/EasyLife/tsconfig.src.json",
  "fileName": "api/src/EasyLife/models/PrintSettings.ts",
  "models": ["PrintMedia"],
  "options": { "type": true, "make": true, "facade": true } }
// response
{ "ok": true, "blocks": { "PrintMedia": "export interface PrintMedia { … }" } }
```

The program forces `fileName` in as a program root, so a model file that a
solution/base tsconfig doesn't directly include is still resolvable.

## Known divergences from the classic resolver

Type-equivalent and deterministic (one-time reformat, then stable):

1. **String-literal union member order** — typescript-go normalizes union
   constituents; the classic checker preserves source order.
2. **Scalar qualification** — native emits the named-import form
   (`NonNegativeNumber`); classic emits the namespace-alias `S.NonNegativeNumber`.
   Both resolve in the target file.

## Performance

~3.1× faster cold than `typescript@6` on the EasyLife leaf project (~440 ms vs
~1330 ms), Encoded-only and full-facade.
