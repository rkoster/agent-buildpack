#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
output_dir="${root}/build"
output="${output_dir}/agent-buildpack.zip"
# shellcheck source=../dependencies.lock
source "${root}/dependencies.lock"

mkdir -p "${output_dir}"
rm -f "${output}"
cd "${root}"
command -v docker >/dev/null || { echo "Docker is required to package the OpenSandbox runtime" >&2; exit 1; }
runtime_tmp=$(mktemp "${output_dir}/opensandbox-capi.XXXXXX")
mecatl_tmp=$(mktemp "${output_dir}/mecatl-runtime.XXXXXX")
studio_container=
studio_rootfs_tar="${output_dir}/studio-rootfs.tar"
cleanup() {
  rm -f "${runtime_tmp}" "${mecatl_tmp}" "${studio_rootfs_tar}"
  if [[ -n "${studio_container}" ]]; then docker rm -f "${studio_container}" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT
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
docker run --rm \
  -v "${root}/runtime/mecatl-runtime:/src:ro" \
  -v "${output_dir}:/out" \
  -w /src golang:1.27 \
  sh -c 'CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mecatl-runtime .'
mv "${output_dir}/opensandbox-capi" "${runtime_tmp}"
mv "${output_dir}/mecatl-runtime" "${mecatl_tmp}"
install -m 0755 "${runtime_tmp}" "${root}/bin/opensandbox-capi"
install -m 0755 "${output_dir}/opencode-database-setup" "${root}/bin/opencode-database-setup"
install -m 0755 "${mecatl_tmp}" "${root}/bin/mecatl-runtime"
rm -f "${output_dir}/opencode-database-setup"
studio_image="${STUDIO_IMAGE}@${STUDIO_DIGEST}"
studio_container=$(docker create "${studio_image}")
docker export --output "${studio_rootfs_tar}" "${studio_container}"
mkdir -p "${output_dir}/studio"
tar -xf "${studio_rootfs_tar}" -C "${output_dir}/studio" app/dist app/web/dist app/node_modules app/package.json
rm -f "${output_dir}/studio/app/node_modules/.pnpm-workspace-state-v1.json" "${output_dir}/studio/app/node_modules/.modules.yaml"
chmod -R a+rX "${output_dir}/studio"
docker rm "${studio_container}" >/dev/null
studio_container=
zip -y -q -r "${output}" bin dependencies.lock manifest.yml opencode README.md build/studio >/dev/null
echo "Created ${output}"
