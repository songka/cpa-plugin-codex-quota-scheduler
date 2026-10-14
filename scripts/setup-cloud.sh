#!/usr/bin/env bash
set -euo pipefail
project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
toolchain_dir=/workspace/toolchains
mkdir -p "$toolchain_dir"
if [[ ! -x "$toolchain_dir/go/bin/go" ]] || [[ $("$toolchain_dir/go/bin/go" version) != 'go version go1.26.9 linux/amd64' ]]; then
  archive=$(mktemp)
  trap 'rm -f -- "$archive"' EXIT
  curl --fail --location --silent --show-error https://dl.google.com/go/go1.26.9.linux-amd64.tar.gz --output "$archive"
  python - "$archive" <<'PY'
import hashlib, sys
with open(sys.argv[1], 'rb') as artifact:
    assert hashlib.sha256(artifact.read()).hexdigest() == '42d158b4d8f7b61ac0a830567c940a86098fb7aac52e467a5ebec03ef5cc2f8d', 'Go artifact checksum mismatch'
PY
  tar -xzf "$archive" -C "$toolchain_dir"
fi
export PATH="$toolchain_dir/go/bin:$PATH"
export GOPATH=/workspace/go-cache GOMODCACHE=/workspace/go-cache/mod GOCACHE=/workspace/go-cache/build GOTOOLCHAIN=local
command -v gcc
command -v make
cd "$project_dir"
# Native Git through the platform proxy remains checksum-verified by Go.
GOPROXY=direct go mod download github.com/router-for-me/CLIProxyAPI/v8
go mod download
go mod verify
