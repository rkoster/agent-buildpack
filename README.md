# OpenCode agent buildpack

This classic Cloud Foundry buildpack detects an app containing a root-level
`AGENTS.md`, installs a pinned OpenCode CLI, and packages a Go
OpenSandbox-compatible lifecycle facade backed by ordinary CAPI Docker apps.
The facade is opt-in until CAPI provisioning credentials are bound.

The facade implements localhost create/list/get/delete/renew-expiration and
endpoint discovery for image-backed apps. Sandbox ports use CF identity-aware
routes and app-specific route policies; endpoint URLs are localhost paths
proxied by the facade, which presents the agent app's instance certificate.
The buildpack also injects an OpenCode plugin with explicit remote sandbox tools.
It does not claim full OpenSandbox compatibility or transparent workspace
synchronization.

When exactly one PostgreSQL service is bound, startup selects its `uri` from
`VCAP_SERVICES`, initializes the schema, and adds the pinned
[`opencode-database-plugin@1.0.12`](https://github.com/aemr3/opencode-database-plugin/tree/53fea738e256fdeed4d42c76b09484680b1fbc47)
to OpenCode's generated config. The URL is passed through
`OPENCODE_DATABASE_URL` to the OpenCode process and is not written into the
generated config. No PostgreSQL binding means no database plugin. This plugin
logs sessions and tool events; OpenCode's native session store remains local.

## Requirements

- Linux x86-64 on the `cflinuxfs4` stack (the binary-only buildpack reports
  support for stacks provisioned with the same glibc-compatible userspace).
- Classic CF buildpack lifecycle support.
- Outbound HTTPS to GitHub Releases during staging.
- Set a non-empty `OPENCODE_SERVER_PASSWORD` application environment variable
  before starting the app. OpenCode's web server is reachable through the app
  route; its built-in HTTP Basic authentication must not be left disabled.
- Bind an OpenCode-supported model/provider credential separately. Model
  bindings are not interpreted by this buildpack yet.
- For the facade, bind a user-provided service named `cf-sandbox-api` with
  `api_url`, `token_url`, `client_id`, `client_secret`, `space_guid`, and
  `sandbox_image`; alternatively configure the corresponding `CF_*` app
  environment variables. `VCAP_APPLICATION` supplies the agent app and space
  GUIDs. The bound OAuth client needs CAPI permissions to
  create/manage Docker apps, routes, route policies, and app metadata in its
  target space. The helper below creates a separate UAA client and never alters
  the existing `ssh-proxy` client grant configuration.
- The configured `CF_IDENTITY_DOMAIN` must be a non-internal domain with
  route-policy enforcement enabled. The lab's `apps.identity` is foundation
  scope. The facade fails closed if it cannot create an owner-only route policy.

## Push a sample app

Register the buildpack with an admin/operator:

```sh
cf create-buildpack agent-buildpack PATH_TO_AGENT_BUILDPACK 99
```

The test lab is targeted at org `poc`, space `demo`, and `cflinuxfs4`. Choose an
unused buildpack position/name as appropriate for the foundation. From an agent
app directory containing `AGENTS.md`, push with:

```sh
cf push my-agent -b agent-buildpack -m 1G -k 2G
cf set-env my-agent OPENCODE_SERVER_PASSWORD 'use-a-long-random-secret'
cf restage my-agent
cf app my-agent
```

The checked-in `fixtures/minimal-agent/manifest.yml` selects `agent-buildpack`.
From the repository root, run:

```sh
cf push opencode-agent-smoke -f fixtures/minimal-agent/manifest.yml \
  -p fixtures/minimal-agent --no-route
```

Set `OPENCODE_SERVER_PASSWORD` before starting if using `--no-start`.

Set model credentials using a supported OpenCode provider environment variable
or an app service binding. Follow the provider's secret handling guidance.
The OpenCode web process currently serves as the primary process. The facade
starts as an opt-in co-process inside that process wrapper when
`OPEN_SANDBOX_API_ENABLED=true`. The smoke test does not exercise an LLM
provider or remote OpenCode tools.

## Buildpack behavior

- `bin/detect`: succeeds only when `<build-dir>/AGENTS.md` is a regular file.
- `bin/supply`: downloads the versioned OpenCode Linux x64 archive and verifies
  its SHA-256 before installing it and the packaged Go facade in the dependency
  directory.
- `bin/finalize`: writes the authenticated OpenCode and co-process startup
  wrapper plus release metadata.
- `bin/release`: selects `./bin/start-agent` as the default web process.

### Injected OpenSandbox tools

The buildpack stages `opencode/plugins/opensandbox.js` into the droplet's
`.opencode/plugins` directory, which OpenCode auto-loads, and disables
OpenCode's local shell and file tools. The plugin provides `sandbox_list`,
`sandbox_create`, `sandbox_command`, `sandbox_read_file`, `sandbox_write_file`,
and `sandbox_delete`. `sandbox_list` shows owned sandboxes and pagination, so
the agent can reuse an existing sandbox ID. It uses the
localhost facade and its endpoint proxy; execd receives
`X-EXECD-ACCESS-TOKEN` when `EXECD_ACCESS_TOKEN` is configured.

### PostgreSQL event logging

Bind a PostgreSQL service that advertises the `postgresql` label or tag and
provides a `credentials.uri` PostgreSQL URL. Startup applies the upstream plugin
schema before enabling the plugin; it stops if a selected binding is incomplete,
the database cannot be reached, or more than one PostgreSQL binding matches.
For this lab's internal DNS, the setup helper resolves the bound host to its
current Silk IP for the plugin process on each agent start; restarting the agent
is required if the database app is recreated with a different IP. The URL and
credentials remain in process memory/environment, not in the OpenCode config.
The schema is copied from upstream revision
`53fea738e256fdeed4d42c76b09484680b1fbc47` in
`runtime/database-setup/schema.sql`. The helper and plugin version are pinned;
the OpenCode runtime fetches the npm plugin on first start. The cf-esb demo's
`PostgreSQL` / `ephemeral` plan returns an internal URL and grants an app-to-app
network policy on bind. That database app is ephemeral, so its data is not
guaranteed across database-app replacement.

At app startup, the static Go facade reads the `dgx-spark-model` user-provided
service binding and merges its selected model/provider into the droplet's
`.opencode/opencode.json`, preserving the buildpack's disabled local tool
settings. Provider credentials therefore stay in the service binding, not in
the app package or manifest.

This is explicit workspace transfer, not a mounted/shared workspace: the
OpenCode app's project directory is not automatically copied into the remote
sandbox. The model must create the sandbox and use the plugin's remote command
and file tools. A later identity-to-JWT implementation should issue scoped
facade/execd credentials rather than placing reusable client secrets in a
foundation-wide variable group.

### CAPI binding schema

Bind a user-provided service named `cf-sandbox-api` with credentials shaped like:

```json
{
  "api_url": "https://api.example.org",
  "token_url": "https://uaa.example.org/oauth/token",
  "client_id": "agent-sandbox-provisioner",
  "client_secret": "...",
  "space_guid": "...",
  "sandbox_image": "registry.example.org/coding-sandbox@sha256:...",
  "open_sandbox_api_key": "optional-localhost-client-key"
}
```

`token_url` may be the UAA base URL or the full `/oauth/token` endpoint.

For a local lab, the same values may be set as `CF_API_URL`, `CF_TOKEN_URL`,
`CF_CLIENT_ID`, `CF_CLIENT_SECRET`, `CF_SPACE_GUID`, and `CF_SANDBOX_IMAGE`.
The app GUID is read from `VCAP_APPLICATION`. Set `CF_IDENTITY_DOMAIN` if the
foundation's identity-aware, route-policy domain is not `apps.identity`.
To provision a Garage `ephemeral` service instance for each Docker sandbox, set
`CF_SANDBOX_WORKSPACE_STORAGE_OFFERING=Garage`. The facade creates an app
credential binding through CAPI v3 before starting the sandbox; Cloud Foundry
provides the binding in the app's `VCAP_SERVICES`. The derived image recognizes
the `workspace-sync` binding and restores/synchronizes `/workspace`. This is
opt-in and only applies to image-based sandbox creates.

Set `OPEN_SANDBOX_API_ENABLED=true` to start the facade. Optionally set
`OPEN_SANDBOX_API_KEY` to require the matching `OPEN-SANDBOX-API-KEY` HTTP
header from clients. The facade binds only to
`127.0.0.1:18080` by default. Configure SDK clients with
`http://127.0.0.1:18080/v1`. Endpoint responses also include the API key header
requirement when a key is configured. Its data-plane proxy presents
`CF_INSTANCE_CERT`/`CF_INSTANCE_KEY` and relies on the route policy. The CAPI
OAuth client is control plane and is never passed to the sandbox app.

To inspect the provisioning plan, then optionally create the `opensandbox-capi`
UAA client and bind it to an already-pushed agent app, use the instant-bosh
operator environment:

```sh
# bosh.env sources the local operator and Config Server environment; instant-bosh
# does not need to be running.
set -a; source ~/workspace/instant-bosh/bosh.env; set +a
./scripts/provision-cf-sandbox-api.sh <agent-app> <sandbox-image>
# Review the dry-run output, then apply explicitly:
./scripts/provision-cf-sandbox-api.sh <agent-app> <sandbox-image> --apply
```

Run the script from the buildpack repo root, where its `scripts/` directory is
present.

The default is a no-change dry run. With `--apply`, the script uses
`uaa_admin_client_secret` to administer UAA and generates an independent secret
for the distinct `opensandbox-capi` client ID. It creates/updates that client
with `client_credentials`; it verifies the
existing `ssh-proxy` grant configuration remains unchanged (`authorization_code`
and, when present, `refresh_token`) and does **not** modify it. Client and
API-key secret files under
`~/.config/cf-opensandbox` are mode `0600`. The UAA admin secret is never put in
the service binding or agent app.

**Lab-only security caveat:** the generated client is granted
`cloud_controller.write` and `cloud_controller.admin` authorities. On the
inspected foundation, `cloud_controller.write` is global and is not restricted
by a CF SpaceDeveloper role; `cloud_controller.admin` is broader still. The
client secret is bound into the agent app. Use only in the isolated POC lab
with trusted prompts. Do not use this client on a shared or production
foundation; a dedicated space-scoped provisioning service/API is needed first.

The headless launch adds a no-op `xdg-open` shim to the process path so OpenCode
does not try to start a desktop browser inside the app container.

The OpenCode version, archive URL, and digest are pinned in `dependencies.lock`.
Update them together and verify the artifact before changing the pin.

## Development and verification

From this repository:

```sh
./scripts/test.sh
./scripts/package.sh
```

`scripts/package.sh` builds the static Go facade in a `golang:1.25.14` Docker
container to avoid depending on host linker/container filesystem compatibility.

The sandbox facade defaults new sandbox apps to a 4096 MiB disk quota. Set
`CF_SANDBOX_DISK_QUOTA_MB` to lower the per-sandbox disk request.
Go facade development uses workspace Devbox Go 1.25:

```sh
devbox run -- sh -c 'cd agent-buildpack/runtime && go test -race ./...'
```

`package.sh` builds the Go facade as a static Linux executable and creates
`build/agent-buildpack.zip`, suitable for `cf create-buildpack`. Package from
the Linux Devbox environment. The target lab verification should use an isolated sample app
name, explicit buildpack, route, password, and resource limits. Delete only
resources created for that test.

## Architecture direction

Keep buildpack lifecycle entrypoints thin and split supply/finalize behavior.
As runtime pieces are added, place compatibility protocol handling, lifecycle
orchestration, CAPI adapters, and data-plane proxies behind independently
testable interfaces. The existing `~/workspace/go-buildpack` is an architectural
reference; its language-specific Go dependency selection is not part of this
buildpack.

## Facade API and current compatibility subset

Base URL: `http://127.0.0.1:18080/v1`. Optional API key authentication uses the
`OPEN-SANDBOX-API-KEY` request header and matching `OPEN_SANDBOX_API_KEY` app
environment variable.

Implemented: `POST/GET /sandboxes`, `GET/DELETE /sandboxes/{id}`,
`POST /sandboxes/{id}/renew-expiration`, endpoint discovery for
`GET /sandboxes/{id}/endpoints/{port}`, and HTTP/WebSocket endpoint proxying
through `/sandboxes/{id}/proxy/{port}/...`. Images must be supplied in
`image.uri`, with an explicit entrypoint. The process command invokes the
derived image's `/opt/opensandbox/bootstrap` before that entrypoint because CF
Docker apps replace the image ENTRYPOINT. Optional image auth is sent to CAPI's
Docker package resource. CAPI limits sandbox metadata to 4.5KB in this POC.

Snapshots, templates, pause/resume, metadata patching, network policy, volumes,
signed endpoints, native raw TCP, and durable persistence are unsupported and
return explicit errors or are not routed. Expiration is reconciled on API list
and get requests; there is no background sweeper while the agent app is down.

The co-process uses the bound OAuth client for CAPI control-plane calls. The
endpoint proxy uses the paths in `CF_INSTANCE_CERT` and `CF_INSTANCE_KEY` for
mTLS to the identity-aware Gorouter route and app-specific route-policy check.
The TLS handshake callback reloads file contents for each new TLS connection.
mTLS terminates at Gorouter; verify backend transport protection separately
on the lab foundation.
