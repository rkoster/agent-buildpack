#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT

mkdir -p "${tmp}/with-agent" "${tmp}/without-agent" "${tmp}/build" "${tmp}/cache" "${tmp}/deps/0"
touch "${tmp}/with-agent/AGENTS.md"

"${root}/bin/detect" "${tmp}/with-agent" > "${tmp}/detect.out"
if "${root}/bin/detect" "${tmp}/without-agent" >/dev/null 2>&1; then
  echo "detect unexpectedly accepted an app without AGENTS.md" >&2
  exit 1
fi

mkdir -p "${tmp}/deps/0/opencode-agent"
touch "${tmp}/deps/0/opencode-agent/opencode"
touch "${tmp}/deps/0/opencode-agent/opensandbox-capi"
chmod +x "${tmp}/deps/0/opencode-agent/opencode" "${tmp}/deps/0/opencode-agent/opensandbox-capi"
"${root}/bin/finalize" "${tmp}/build" "${tmp}/cache" "${tmp}/deps" 0

grep -q 'OPENCODE_SERVER_PASSWORD must be set' "${tmp}/build/bin/start-opencode"
grep -q 'OPENCODE_SERVER_PASSWORD must be set' "${tmp}/build/bin/start-agent"
grep -q 'OPEN_SANDBOX_API_ENABLED' "${tmp}/build/bin/start-agent"
grep -q 'sandbox_create' "${root}/opencode/plugins/opensandbox.js"
grep -q 'sandbox_list' "${root}/opencode/plugins/opensandbox.js"
grep -q 'sandbox_command' "${root}/opencode/plugins/opensandbox.js"
test -f "${tmp}/build/.opencode/plugins/opensandbox.js"
grep -q '"bash": false' "${tmp}/build/.opencode/opencode.json"
grep -q 'cleanup_facade' "${tmp}/build/bin/start-agent"
grep -q 'facade did not become ready' "${tmp}/build/bin/start-agent"
test -x "${tmp}/build/bin/xdg-open"
"${tmp}/build/bin/xdg-open" http://localhost:8080
grep -Fq '${DEPS_DIR}/0/opencode-agent/opencode' "${tmp}/build/bin/start-opencode"
if grep -q 'DEPS_IDX' "${tmp}/build/bin/start-opencode"; then
  echo "startup script must not rely on an unset DEPS_IDX variable" >&2
  exit 1
fi
grep -q 'default_process_types:' "${tmp}/build/release.yml"
grep -q 'web: ./bin/start-agent' "${tmp}/build/release.yml"
"${root}/bin/release" "${tmp}/build" > "${tmp}/release.out"
grep -q 'web: ./bin/start-agent' "${tmp}/release.out"
node --check "${root}/opencode/plugins/opensandbox.js"
node "${root}/scripts/test-opencode-plugin.mjs"
python3 -c 'from pathlib import Path; compile(Path("scripts/provision-cf-sandbox-api.py").read_text(), "provision-cf-sandbox-api.py", "exec")'
python3 scripts/test-provisioner.py

echo "Buildpack unit checks passed"
