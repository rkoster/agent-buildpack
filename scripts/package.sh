#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
output_dir="${root}/build"
output="${output_dir}/agent-buildpack.zip"

mkdir -p "${output_dir}"
rm -f "${output}"
cd "${root}"
command -v docker >/dev/null || { echo "Docker is required to package the OpenSandbox runtime" >&2; exit 1; }
runtime_tmp=$(mktemp "${output_dir}/opensandbox-capi.XXXXXX")
trap 'rm -f "${runtime_tmp}"' EXIT
docker run --rm \
  -v "${root}/runtime:/src:ro" \
  -v "${output_dir}:/out" \
  -w /src golang:1.25.14 \
  sh -c 'CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/opensandbox-capi .'
docker run --rm \
  -v "${root}/runtime:/src:ro" \
  -v "${output_dir}:/out" \
  -w /src golang:1.25.14 \
  sh -c 'CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/opencode-database-setup ./database-setup'
mv "${output_dir}/opensandbox-capi" "${runtime_tmp}"
install -m 0755 "${runtime_tmp}" "${root}/bin/opensandbox-capi"
install -m 0755 "${output_dir}/opencode-database-setup" "${root}/bin/opencode-database-setup"
rm -f "${output_dir}/opencode-database-setup"
rm -f "${runtime_tmp}"
zip -q -r "${output}" bin dependencies.lock manifest.yml opencode README.md >/dev/null
echo "Created ${output}"
