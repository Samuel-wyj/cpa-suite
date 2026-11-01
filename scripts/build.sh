#!/usr/bin/env bash
# Cross-build cpa-suite with CGO via zig and package store-format zips.
#   scripts/build.sh [platform ...]   default: linux/amd64 linux/arm64 windows/amd64
# Output: ../dist/cpa-suite_<version>_<goos>_<goarch>.zip + checksums.txt
# darwin/* builds run on macOS (see .github/workflows/release.yml).
set -euo pipefail
cd "$(dirname "$0")/.."
id=cpa-suite
version=$(sed -n 's/^[[:space:]]*Version[[:space:]]*= "\([0-9.]*\)"/\1/p' internal/suite/config.go)
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "bad version '$version'" >&2; exit 1; }
out=${OUT_DIR:-../dist}; mkdir -p "$out"
export ZIG_GLOBAL_CACHE_DIR=${ZIG_GLOBAL_CACHE_DIR:-$PWD/.zig-cache}
platforms=("$@"); [ ${#platforms[@]} -gt 0 ] || platforms=(linux/amd64 linux/arm64 windows/amd64)
for p in "${platforms[@]}"; do
  goos=${p%/*} goarch=${p#*/}
  case $p in
    linux/amd64)   cc='zig cc -target x86_64-linux-gnu.2.17'; ext=so ;;
    linux/arm64)   cc='zig cc -target aarch64-linux-gnu.2.17'; ext=so ;;
    windows/amd64) cc='zig cc -target x86_64-windows-gnu'; ext=dll ;;
    darwin/*)      cc=${CC:-clang}; ext=dylib ;;
    *) echo "unsupported $p" >&2; exit 1 ;;
  esac
  b=build/$goos/$goarch; rm -rf "$b"; mkdir -p "$b"
  echo "==> $p"
  GOOS=$goos GOARCH=$goarch CGO_ENABLED=1 CC="$cc" \
    go build -trimpath -ldflags='-s -w' -tags cshared -buildmode=c-shared -o "$b/$id.$ext" ./cmd/$id
  rm -f "$b/$id.h"
  python3 - "$out/${id}_${version}_${goos}_${goarch}.zip" "$b/$id.$ext" <<'PY'
import sys, zipfile, os
z, f = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(z, "w", zipfile.ZIP_DEFLATED) as w:
    i = zipfile.ZipInfo(os.path.basename(f), (2026, 1, 1, 0, 0, 0)); i.external_attr = 0o755 << 16; i.compress_type = zipfile.ZIP_DEFLATED
    w.writestr(i, open(f, "rb").read())
PY
done
(cd "$out" && { command -v sha256sum >/dev/null && sha256sum ${id}_${version}_*.zip || shasum -a 256 ${id}_${version}_*.zip; } > checksums.txt && cat checksums.txt)
