#!/usr/bin/env bash
# Rebuild the committed forked-tsgolint binaries.
#
# Reproduces the fork (pinned tsgolint commit + typescript-go submodule + its
# patches), drops in our `model-codegen` subcommand, cross-compiles the platform
# matrix (pure Go, CGO off), and writes gzipped binaries into ./bin/. Only the
# .gz archives are committed; the bin/tsgolint.js shim gunzips per platform on
# first run.
#
#   ./build.sh         # host platform only
#   ./build.sh --all   # full matrix (commit these)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TSGOLINT_COMMIT="d4d312edc13b682a1869b1bc4f6130a5de89a16f"
WORK="${HERE}/.work/tsgolint"
OUT="${HERE}/bin"

command -v go >/dev/null || { echo "go toolchain required" >&2; exit 1; }

if [ ! -d "${WORK}/.git" ]; then
  rm -rf "${WORK}"
  git clone https://github.com/oxc-project/tsgolint.git "${WORK}"
fi
cd "${WORK}"
git fetch --depth 1 origin "${TSGOLINT_COMMIT}"
git checkout -q "${TSGOLINT_COMMIT}"
git submodule update --init --depth 1
( cd typescript-go && git am --3way --no-gpg-sign ../patches/*.patch 2>/dev/null || true )
mkdir -p internal/collections
find ./typescript-go/internal/collections -type f ! -name '*_test.go' -exec cp {} internal/collections/ \;

# Our subcommand + its dispatch in main().
cp "${HERE}/cmd/model_codegen.go" cmd/tsgolint/model_codegen.go
if ! grep -q 'os.Args\[1\] == "model-codegen"' cmd/tsgolint/main.go; then
  perl -0pi -e 's/(if len\(os\.Args\) > 1 && os\.Args\[1\] == "headless" \{\n\t\treturn runHeadless\(os\.Args\[2:\]\)\n\t\}\n)/$1\n\tif len(os.Args) > 1 \&\& os.Args[1] == "model-codegen" {\n\t\treturn runModelCodegen(os.Args[2:])\n\t}\n/' cmd/tsgolint/main.go
fi

mkdir -p "${OUT}"
build_one() { # node_plat node_arch goos goarch
  local exe=""; [ "$1" = "win32" ] && exe=".exe"
  local name="tsgolint-codegen-$1-$2${exe}"
  echo "building ${name} (GOOS=$3 GOARCH=$4)"
  CGO_ENABLED=0 GOOS="$3" GOARCH="$4" go build -o "${OUT}/${name}" ./cmd/tsgolint
  gzip -9 -f "${OUT}/${name}"   # -> ${name}.gz
}

if [ "${1:-}" = "--all" ]; then
  build_one linux  x64   linux   amd64
  build_one linux  arm64 linux   arm64
  build_one darwin x64   darwin  amd64
  build_one darwin arm64 darwin  arm64
  build_one win32  x64   windows amd64
  build_one win32  arm64 windows arm64
else
  case "$(go env GOOS)" in linux) P=linux;; darwin) P=darwin;; windows) P=win32;; esac
  case "$(go env GOARCH)" in amd64) A=x64;; arm64) A=arm64;; esac
  build_one "${P}" "${A}" "$(go env GOOS)" "$(go env GOARCH)"
fi
echo "wrote gzipped binaries to ${OUT}"
