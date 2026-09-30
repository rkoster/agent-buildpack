#!/usr/bin/env bash
set -euo pipefail
set +x

usage() {
  printf 'Usage: %s AGENT_APP_NAME SANDBOX_IMAGE [--apply]\n' "$0" >&2
  printf 'Optional: IBOSH_ROOT, IBOSH_BACKEND=docker|incus, CF_ORG, CF_SPACE, CF_IDENTITY_DOMAIN\n' >&2
}

if [[ $# -lt 2 || $# -gt 3 ]]; then
  usage
  exit 2
fi

app_name=$1
sandbox_image=$2
apply_flag=${3:-}
if [[ -n "${apply_flag}" && "${apply_flag}" != "--apply" ]]; then
  usage
  exit 2
fi
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
workspace_root=$(cd "${script_dir}/../.." && pwd)
ibosh_root=${IBOSH_ROOT:-"${HOME}/workspace/instant-bosh"}
ibosh_backend=${IBOSH_BACKEND:-docker}

case "${ibosh_backend}" in
  docker|incus) ;;
  *) printf 'IBOSH_BACKEND must be docker or incus\n' >&2; exit 2 ;;
esac

for tool in python3 cf; do
  command -v "${tool}" >/dev/null 2>&1 || { printf 'Required command not found: %s\n' "${tool}" >&2; exit 1; }
done
[[ -d "${ibosh_root}" ]] || { printf 'instant-bosh repo not found: %s\n' "${ibosh_root}" >&2; exit 1; }

ibosh() {
  if command -v go >/dev/null 2>&1; then
    (cd "${ibosh_root}" && go run ./cmd/ibosh/main.go "$@")
  elif command -v devbox >/dev/null 2>&1; then
    (cd "${ibosh_root}" && devbox run -- go run ./cmd/ibosh/main.go "$@")
  else
    printf 'Go or Devbox is required to run instant-bosh\n' >&2
    return 127
  fi
}

# bosh.env is the local operator environment and works independently of
# whether the instant-bosh container is running. Keep all credentials private.
if [[ -f "${ibosh_root}/bosh.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "${ibosh_root}/bosh.env"
  set +a
else
  env_script=$(ibosh "${ibosh_backend}" print-env)
  eval "${env_script}"
  unset env_script
fi

if [[ -z "${UAA_URL:-}" || -z "${UAA_CA_CERT:-}" ]]; then
  printf 'ibosh print-env did not provide UAA_URL/UAA_CA_CERT\n' >&2
  exit 1
fi

# Retrieve the UAA administration secret while bosh.env still has the CA
# needed by instant-bosh's Config Server client.
export UAA_ADMIN_SECRET
UAA_ADMIN_SECRET=$(ibosh creds get /instant-bosh/cf/uaa_admin_client_secret)
[[ -n "${UAA_ADMIN_SECRET}" ]] || { printf 'Config Server returned an empty UAA admin client secret\n' >&2; exit 1; }

# The local bosh.env may predate the currently targeted CF deployment. Prefer
# the token endpoint advertised by the connected CAPI foundation, retaining the
# configured CA and using CF's public trust roots for the active ingress host.
api_output=$(cf api)
api_url=$(printf '%s\n' "${api_output}" | python3 -c 'import re,sys; m=re.search(r"^API endpoint:\s*(\S+)",sys.stdin.read(),re.M); print(m.group(1) if m else "")')
if [[ -n "${api_url}" ]]; then
  info=$(cf curl /v2/info)
  active_uaa=$(printf '%s' "${info}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token_endpoint", ""))')
fi
if [[ -n "${api_url}" ]]; then
  # CAPI transport uses the operator-configured system roots in cf, so switch
  # the helper to the active platform UAA endpoint instead of the stale
  # director endpoint and its unrelated CA. Retrieve the platform router CA
  # for the active CF ingress and pass that as a temporary local trust file.
  if command -v go >/dev/null 2>&1; then
    router_ca_json=$(cd "${ibosh_root}" && go run ./cmd/ibosh/main.go creds get --json /instant-bosh/cf/router_ca)
  else
    router_ca_json=$(cd "${ibosh_root}" && devbox run -- go run ./cmd/ibosh/main.go creds get --json /instant-bosh/cf/router_ca)
  fi
  router_ca=$(printf '%s' "${router_ca_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("ca", ""))')
  unset router_ca_json
  [[ -n "${router_ca}" ]] || { printf 'Could not retrieve CF router trust CA\n' >&2; exit 1; }
  PROVISIONER_UAA_CA_FILE=$(mktemp)
  chmod 600 "${PROVISIONER_UAA_CA_FILE}"
  printf '%s\n' "${router_ca}" > "${PROVISIONER_UAA_CA_FILE}"
  unset router_ca
  UAA_URL=${active_uaa}
  UAA_CA_CERT=""
  trap 'rm -f "${PROVISIONER_UAA_CA_FILE}"' EXIT
fi
export UAA_URL UAA_CA_CERT PROVISIONER_UAA_CA_FILE
export UAA_INSECURE_TLS=0
unset CONFIG_SERVER_SECRET

export CF_AGENT_APP_NAME="${app_name}"
export CF_SANDBOX_IMAGE="${sandbox_image}"
export CF_IDENTITY_DOMAIN="${CF_IDENTITY_DOMAIN:-apps.identity}"
export PROVISIONER_SECRET_DIR="${PROVISIONER_SECRET_DIR:-${HOME}/.config/cf-opensandbox}"
export PROVISIONER_BINDING_NAME="${PROVISIONER_BINDING_NAME:-cf-sandbox-api-${app_name}}"

if [[ -n "${apply_flag}" ]]; then
  exec python3 "${script_dir}/provision-cf-sandbox-api.py" --apply
fi
exec python3 "${script_dir}/provision-cf-sandbox-api.py"
