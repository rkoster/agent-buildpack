#!/usr/bin/env python3
"""Provision a client_credentials UAA client and bind it to a CF app.

The UAA admin secret is sourced from instant-bosh config-server by the shell
wrapper. Client and API-key secrets are written to mode-0600 local files and
passed to cf through a mode-0600 temporary credential file; secrets are never
printed or placed in argv.
"""

from __future__ import annotations

import base64
import json
import os
import re
import secrets
import socket
import ssl
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, NoReturn


CLIENT_ID = "opensandbox-capi"
CLIENT_SCOPES = ["openid", "cloud_controller.read", "cloud_controller.write", "cloud_controller.admin"]
CLIENT_AUTHORITIES = ["cloud_controller.read", "cloud_controller.write", "cloud_controller.admin"]


def fail(message: str) -> NoReturn:
    print(f"provision-cf-sandbox-api: {message}", file=sys.stderr)
    raise SystemExit(1)


def run(argv: list[str]) -> subprocess.CompletedProcess[str]:
    return subprocess.run(argv, text=True, capture_output=True, check=False)


def call_cf(*args: str) -> str:
    result = run(["cf", *args])
    if result.returncode:
        # CF output may include values from service credentials. Do not echo it.
        fail(f"cf {' '.join(args[:2])} failed (exit {result.returncode})")
    return result.stdout


def target_values() -> tuple[str, str]:
    output = call_cf("target")
    org_match = re.search(r"^org:\s*(\S+)", output, re.MULTILINE)
    space_match = re.search(r"^space:\s*(\S+)", output, re.MULTILINE)
    org = os.environ.get("CF_ORG") or (org_match.group(1) if org_match else "")
    space = os.environ.get("CF_SPACE") or (space_match.group(1) if space_match else "")
    if not org or not space:
        fail("target a CF org and space, or set CF_ORG and CF_SPACE")
    return org, space


def api_url_from_cli() -> str:
    output = call_cf("api")
    match = re.search(r"^API endpoint:\s*(\S+)", output, re.MULTILINE)
    if not match:
        fail("could not determine CAPI URL from cf api")
    return match.group(1).rstrip("/")


def json_from_cf(path: str) -> dict[str, Any]:
    try:
        value = json.loads(call_cf("curl", path))
    except json.JSONDecodeError:
        fail("cf curl returned invalid JSON")
    if not isinstance(value, dict):
        fail("cf curl returned an unexpected JSON value")
    return value


def target_app_guid(app_name: str) -> str:
    result = run(["cf", "app", app_name, "--guid"])
    if result.returncode:
        fail(f"target app {app_name!r} does not exist in the current CF space")
    guid = result.stdout.strip()
    if not guid:
        fail("cf app did not return an app GUID")
    return guid


def target_app_routes(app_name: str) -> set[str]:
    result = json_from_cf(f"/v3/apps/{urllib.parse.quote(target_app_guid(app_name), safe='')}/routes?per_page=100")
    return {str(route.get("guid", "")) for route in result.get("resources", []) if route.get("guid")}


def target_space_guid(org_name: str, space_name: str) -> str:
    orgs = json_from_cf(f"/v3/organizations?names={urllib.parse.quote(org_name)}&per_page=100").get("resources", [])
    org_guids = {org.get("guid") for org in orgs if org.get("name") == org_name}
    spaces = json_from_cf(f"/v3/spaces?names={urllib.parse.quote(space_name)}&per_page=100").get("resources", [])
    matches = [
        space for space in spaces
        if space.get("name") == space_name
        and space.get("relationships", {}).get("organization", {}).get("data", {}).get("guid") in org_guids
    ]
    if len(matches) != 1:
        fail("target space GUID lookup was ambiguous")
    return matches[0]["guid"]


def tls_context() -> ssl.SSLContext:
    ca = os.environ.get("UAA_CA_CERT", "").strip()
    if ca:
        return ssl.create_default_context(cadata=ca)
    ca_file = os.environ.get("PROVISIONER_UAA_CA_FILE", "").strip()
    if ca_file:
        return ssl.create_default_context(cafile=ca_file)
    fail("UAA_CA_CERT or PROVISIONER_UAA_CA_FILE is required to verify UAA TLS")


def verify_tls_context(context: ssl.SSLContext, uaa_url: str) -> None:
    host = urllib.parse.urlparse(uaa_url).hostname
    if not host:
        fail("UAA URL is invalid")
    try:
        with socket.create_connection((host, 443), timeout=10) as conn:
            with context.wrap_socket(conn, server_hostname=host):
                pass
    except Exception as error:
        fail(f"UAA certificate verification failed for {host}: {error}")


def verify_identity_domain(output: str, name: str) -> bool:
    rows = [line for line in output.splitlines() if name in line.split()]
    return bool(rows) and any("enforced" in line.lower() for line in rows)


def uaa_request(
    context: ssl.SSLContext,
    url: str,
    *,
    method: str = "GET",
    payload: dict[str, Any] | None = None,
    bearer: str | None = None,
) -> tuple[int, dict[str, Any]]:
    body = json.dumps(payload).encode() if payload is not None else None
    headers = {"Accept": "application/json"}
    if bearer:
        headers["Authorization"] = f"Bearer {bearer}"
    if body is not None:
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(url, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, context=context, timeout=30) as response:
            raw = response.read()
            return response.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as error:
        try:
            raw = error.read()
            parsed = json.loads(raw) if raw else {}
        except (OSError, json.JSONDecodeError):
            parsed = {}
        return error.code, parsed
    except (OSError, TimeoutError):
        fail("UAA request failed (network/TLS error)")


def get_admin_token(context: ssl.SSLContext, uaa_url: str, secret: str) -> str:
    token_url = uaa_url.rstrip("/")
    if not token_url.endswith("/oauth/token"):
        token_url += "/oauth/token"
    basic = base64.b64encode(f"admin:{secret}".encode()).decode()
    secret = ""
    request = urllib.request.Request(
        token_url,
        data=urllib.parse.urlencode({"grant_type": "client_credentials"}).encode(),
        headers={
            "Accept": "application/json",
            "Content-Type": "application/x-www-form-urlencoded",
            "Authorization": "Basic " + basic,
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, context=context, timeout=30) as response:
            payload = json.loads(response.read())
            token = payload.get("access_token", "")
            scopes = set(str(payload.get("scope", "")).split())
    except urllib.error.HTTPError as error:
        fail(f"could not obtain UAA administration token (HTTP {error.code}); check operator credentials")
    except Exception as error:
        if isinstance(error, urllib.error.URLError):
            fail(f"could not obtain UAA administration token (TLS/network error): {error.reason}")
        fail("could not obtain UAA administration token; check instant-bosh config-server environment")
    if not token:
        fail("UAA token response omitted access_token")
    if "clients.read" not in scopes or "clients.write" not in scopes:
        fail("UAA admin client token lacks clients.read/clients.write; cannot provision OAuth clients")
    return token


def save_secret(directory: Path, name: str, value: str, *, replace: bool = False) -> str:
    path = directory / name
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(directory, 0o700)
    if path.exists() and not replace:
        try:
            existing = path.read_text().strip()
        except OSError:
            fail(f"could not read local secret {path}")
        if existing:
            return existing
    if path.exists() and replace:
        try:
            existing = path.read_text().strip()
        except OSError:
            fail(f"could not read local secret file {path}")
        if existing == value:
            return existing
    secret = value
    if not secret:
        fail(f"refusing to write an empty local secret file {path}")
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(path, flags, 0o600)
        with os.fdopen(fd, "w") as stream:
            stream.write(secret + "\n")
        os.chmod(path, 0o600)
    except OSError:
        fail(f"could not store local secret {path}")
    return secret


def read_secret(directory: Path, name: str) -> str:
    path = directory / name
    if not path.exists():
        return ""
    try:
        return path.read_text().strip()
    except OSError:
        fail(f"could not read local secret {path}")


def ensure_client(context: ssl.SSLContext, uaa_url: str, admin_token: str, secret: str, *, apply: bool = False) -> None:
    client = {
        "client_id": CLIENT_ID,
        "name": "OpenSandbox CAPI facade provisioner",
        "authorized_grant_types": ["client_credentials"],
        "scope": CLIENT_SCOPES,
        "authorities": CLIENT_AUTHORITIES,
        "resource_ids": ["none"],
        "redirect_uri": ["https://uaa.example.invalid/login"],
        "autoapprove": True,
    }
    root = uaa_url.rstrip("/") + "/oauth/clients"
    status, existing = uaa_request(context, root + "/" + CLIENT_ID, bearer=admin_token)
    if status == 404:
        verify_existing_grants(context, root, admin_token)
        if not apply:
            print("UAA client opensandbox-capi is missing; rerun with --apply to create it.", file=sys.stderr)
            return
        create_payload = dict(client)
        create_payload["client_secret"] = secret
        status, _ = uaa_request(context, root, method="POST", payload=create_payload, bearer=admin_token)
        if status != 201:
            fail(f"UAA client creation failed (HTTP {status})")
        status, created = uaa_request(context, root + "/" + CLIENT_ID, bearer=admin_token)
        if status != 200 or set(created.get("authorized_grant_types", [])) != {"client_credentials"}:
            fail("new opensandbox-capi UAA client grant verification failed")
        if not set(CLIENT_SCOPES).issubset(set(created.get("scope", []))):
            fail("new opensandbox-capi UAA client is missing a requested scope")
        verify_existing_grants(context, root, admin_token)
        return
    if status != 200:
        fail(f"UAA client lookup failed (HTTP {status})")
    if not apply:
        grants = set(existing.get("authorized_grant_types", []))
        if grants == {"client_credentials"} and set(CLIENT_SCOPES).issubset(set(existing.get("scope", []))):
            print("UAA client opensandbox-capi already has the expected client_credentials grant and CAPI scopes.", file=sys.stderr)
        else:
            print("UAA client opensandbox-capi needs a client_credentials grant/scopes update; rerun with --apply.", file=sys.stderr)
        verify_existing_grants(context, root, admin_token)
        return
    existing_grants = set(existing.get("authorized_grant_types", []))
    if existing_grants & {"password", "authorization_code", "implicit"}:
        fail("refusing to convert an interactive user client; choose a different client ID")
    status, _ = uaa_request(context, root + "/" + CLIENT_ID, method="PUT", payload=client, bearer=admin_token)
    if status != 200:
        fail(f"UAA client configuration update failed (HTTP {status})")
    status, updated = uaa_request(context, root + "/" + CLIENT_ID, bearer=admin_token)
    if status != 200:
        fail(f"UAA client lookup after update failed (HTTP {status})")
    grants = set(updated.get("authorized_grant_types", []))
    if "client_credentials" not in grants or "authorization_code" in grants:
        fail("opensandbox-capi UAA client must contain client_credentials only")
    if not set(CLIENT_SCOPES).issubset(set(updated.get("scope", []))):
        fail("opensandbox-capi UAA client is missing a requested scope")
    secret_status, _ = uaa_request(
        context,
        root + "/" + CLIENT_ID + "/secret",
        method="PUT",
        payload={"clientId": CLIENT_ID, "secret": secret},
        bearer=admin_token,
    )
    if secret_status != 200:
        fail(f"UAA client secret rotation failed (HTTP {secret_status})")
    verify_existing_grants(context, root, admin_token)


def verify_existing_grants(context: ssl.SSLContext, root: str, admin_token: str) -> None:
    status, ssh_proxy = uaa_request(context, root + "/ssh-proxy", bearer=admin_token)
    if status != 200:
        fail("could not verify that the ssh-proxy client remains present")
    expected_scopes = {"openid", "cloud_controller.read", "cloud_controller.write", "cloud_controller.admin"}
    grants = set(ssh_proxy.get("authorized_grant_types", []))
    if grants not in ({"authorization_code"}, {"authorization_code", "refresh_token"}):
        fail("ssh-proxy grant configuration changed unexpectedly; verify the deployment manifest")
    if set(ssh_proxy.get("scope", [])) != expected_scopes:
        fail("ssh-proxy scopes differ from the expected lab configuration; refusing to proceed")


def plan_client(context: ssl.SSLContext, uaa_url: str, admin_token: str) -> None:
    root = uaa_url.rstrip("/") + "/oauth/clients"
    status, client = uaa_request(context, root + "/" + CLIENT_ID, bearer=admin_token)
    if status not in (200, 404):
        fail(f"UAA client inspection failed (HTTP {status})")
    verify_existing_grants(context, root, admin_token)
    if status == 404:
        print("Plan: create opensandbox-capi with client_credentials and CAPI scopes.")
    else:
        grants = set(client.get("authorized_grant_types", []))
        wanted = {"client_credentials"}
        if grants & {"password", "authorization_code", "implicit"}:
            fail("refusing to repurpose an interactive user client; choose a new machine client ID")
        if grants == wanted and set(CLIENT_SCOPES).issubset(set(client.get("scope", []))):
            print("The existing client already has the requested grant and scopes.")
        else:
            print("Plan: update opensandbox-capi to the requested client_credentials grant and scopes.")


def verify_client_credentials(context: ssl.SSLContext, uaa_url: str, secret: str) -> None:
    token_url = uaa_url.rstrip("/")
    if not token_url.endswith("/oauth/token"):
        token_url += "/oauth/token"
    token_req = urllib.request.Request(
        token_url,
        data=urllib.parse.urlencode({"grant_type": "client_credentials"}).encode(),
        headers={
            "Accept": "application/json",
            "Content-Type": "application/x-www-form-urlencoded",
            "Authorization": "Basic " + base64.b64encode(f"{CLIENT_ID}:{secret}".encode()).decode(),
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(token_req, context=context, timeout=30) as response:
            payload = json.loads(response.read())
    except Exception:
        fail("UAA client_credentials verification failed")
    scopes = set(str(payload.get("scope", "")).split())
    # openid is a UAA client registration scope but is not returned in a
    # client_credentials token; require only the actual CAPI authorities.
    required = set(CLIENT_AUTHORITIES)
    if not payload.get("access_token") or not required.issubset(scopes):
        fail("UAA client token is missing required CAPI scopes")


def main() -> None:
    unexpected = [arg for arg in sys.argv[1:] if arg != "--apply"]
    if unexpected:
        fail("only the optional --apply flag is accepted by this helper")
    apply_changes = "--apply" in sys.argv[1:]
    app_name = os.environ.get("CF_AGENT_APP_NAME", "").strip()
    image = os.environ.get("CF_SANDBOX_IMAGE", "").strip()
    uaa_url = os.environ.get("UAA_URL", "").strip()
    admin_secret = os.environ.get("UAA_ADMIN_SECRET", "")
    if not app_name or not image or not uaa_url or not admin_secret:
        fail("agent app, sandbox image, UAA URL, and UAA admin credential are required")

    if not apply_changes:
        print("PROVISIONER DRY RUN: UAA and CF resources will not be changed.", file=sys.stderr)

    org, space = target_values()
    agent_guid = target_app_guid(app_name)
    cf_api_url = os.environ.get("CF_API_URL", "").strip() or api_url_from_cli()
    cf_space_guid = os.environ.get("CF_SPACE_GUID", "").strip() or target_space_guid(org, space)
    identity_domain = os.environ.get("CF_IDENTITY_DOMAIN", "apps.identity")
    domains_output = call_cf("domains")
    if not verify_identity_domain(domains_output, identity_domain):
        fail(f"CF domain {identity_domain!r} is not visible with route-policy enforcement")

    secret_dir = Path(os.environ.get("PROVISIONER_SECRET_DIR", "~/.config/cf-opensandbox")).expanduser()
    if apply_changes:
        secret_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        os.chmod(secret_dir, 0o700)
    if apply_changes:
        client_secret = save_secret(
            secret_dir,
            "opensandbox-capi.secret",
            read_secret(secret_dir, "opensandbox-capi.secret") or secrets.token_urlsafe(48),
        )
        api_key = save_secret(secret_dir, "opensandbox-api-key.secret", os.environ.get("OPEN_SANDBOX_API_KEY") or secrets.token_urlsafe(32))
        ca_file = os.environ.get("PROVISIONER_UAA_CA_FILE", "")
        ca_cert = Path(ca_file).read_text() if ca_file else os.environ.get("UAA_CA_CERT", "")
    else:
        client_secret = read_secret(secret_dir, "opensandbox-capi.secret") or "DRY-RUN-ONLY"
        api_key = read_secret(secret_dir, "opensandbox-api-key.secret") or "DRY-RUN-ONLY"
        ca_file = os.environ.get("PROVISIONER_UAA_CA_FILE", "")
        ca_cert = Path(ca_file).read_text() if ca_file else os.environ.get("UAA_CA_CERT", "")

    context = tls_context()
    verify_tls_context(context, uaa_url)
    admin_token = get_admin_token(context, uaa_url, admin_secret)
    admin_secret = ""
    os.environ.pop("UAA_ADMIN_SECRET", None)
    plan_client(context, uaa_url, admin_token)
    if not apply_changes:
        admin_token = ""
        print("Dry run complete; no secrets, UAA clients, or CF resources were changed. Pass --apply to continue.")
        return
    ensure_client(context, uaa_url, admin_token, client_secret, apply=apply_changes)
    admin_token = ""
    verify_client_credentials(context, uaa_url, client_secret)
    print(
        "WARNING: client_credentials provisioning requires an operator-approved UAA/CAPI authorization mapping. "
        "Do not enable global cloud_controller.admin authority on shared or production foundations.",
        file=sys.stderr,
    )

    binding_name = os.environ.get("PROVISIONER_BINDING_NAME", f"cf-sandbox-api-{app_name}")
    with tempfile.TemporaryDirectory(prefix="cf-sandbox-binding-") as temp_dir:
        os.chmod(temp_dir, 0o700)
        binding_file = Path(temp_dir) / "credentials.json"
        binding = {
            "api_url": cf_api_url,
            "token_url": uaa_url,
            "client_id": CLIENT_ID,
            "client_secret": client_secret,
            "space_guid": cf_space_guid,
            "sandbox_image": image,
            "disk_quota_mb": int(os.environ.get("CF_SANDBOX_DISK_QUOTA_MB", "4096")),
            "identity_domain": identity_domain,
            "ca_cert": ca_cert,
            "open_sandbox_api_key": api_key,
        }
        binding_file.write_text(json.dumps(binding))
        os.chmod(binding_file, 0o600)
        service = run(["cf", "service", binding_name])
        if service.returncode:
            result = run(["cf", "create-user-provided-service", binding_name, "-p", str(binding_file)])
        else:
            result = run(["cf", "update-user-provided-service", binding_name, "-p", str(binding_file)])
        if result.returncode:
            fail("could not create/update the CAPI user-provided service")

    app_env = call_cf("env", app_name)
    if binding_name not in app_env:
        result = run(["cf", "bind-service", app_name, binding_name])
        if result.returncode:
            fail("could not bind the CAPI service to the agent app")
    for key, value in {
        "OPEN_SANDBOX_API_ENABLED": "true",
        "OPEN_SANDBOX_API_KEY": api_key,
        "CF_API_URL": cf_api_url,
        "CF_SPACE_GUID": cf_space_guid,
        "CF_SANDBOX_IMAGE": image,
        "CF_IDENTITY_DOMAIN": identity_domain,
    }.items():
        result = run(["cf", "set-env", app_name, key, value])
        if result.returncode:
            fail(f"could not set {key} on the agent app")
    result = run(["cf", "restage", app_name])
    if result.returncode:
        fail("agent app restage failed")
    print(f"Created/updated OAuth client {CLIENT_ID} and bound {binding_name} to {app_name}.")
    print(f"Generated client/API-key secrets are stored in {secret_dir} with private permissions.")


if __name__ == "__main__":
    main()
