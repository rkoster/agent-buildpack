#!/usr/bin/env python3
"""Small no-network tests for the UAA client provisioning helper."""

import importlib.util
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("provision-cf-sandbox-api.py")
SPEC = importlib.util.spec_from_file_location("provision_cf_sandbox_api", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("unable to load provisioning helper module")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class ProvisionerTests(unittest.TestCase):
    def test_client_create_uses_machine_grant_and_capi_scopes(self):
        calls = []
        client_lookups = 0

        def request(_context, url, **kwargs):
            nonlocal client_lookups
            calls.append((url, kwargs))
            if url.endswith("/ssh-proxy"):
                return 200, {"authorized_grant_types": ["authorization_code"], "scope": ["openid", "cloud_controller.read", "cloud_controller.write", "cloud_controller.admin"]}
            if url.endswith("/opensandbox-capi"):
                client_lookups += 1
                if client_lookups == 1:
                    return 404, {}
                return 200, {"authorized_grant_types": ["client_credentials"], "scope": MODULE.CLIENT_SCOPES, "authorities": MODULE.CLIENT_AUTHORITIES}
            if url.endswith("/oauth/clients"):
                return 201, {"authorized_grant_types": ["client_credentials"], "scope": MODULE.CLIENT_SCOPES, "authorities": MODULE.CLIENT_AUTHORITIES}
            return 201, {}

        with patch.object(MODULE, "uaa_request", side_effect=request):
            MODULE.ensure_client(object(), "https://uaa.test", "admin-token", "new-secret", apply=True)

        create_call = next(call for call in calls if call[0] == "https://uaa.test/oauth/clients")
        payload = create_call[1]["payload"]
        self.assertEqual(payload["client_secret"], "new-secret")
        self.assertEqual(payload["authorized_grant_types"], ["client_credentials"])
        self.assertEqual(payload["scope"], MODULE.CLIENT_SCOPES)
        self.assertEqual(payload["authorities"], MODULE.CLIENT_AUTHORITIES)
        self.assertIn("cloud_controller.admin", payload["scope"])
        self.assertTrue(any(call[0].endswith("/ssh-proxy") for call in calls))
        self.assertNotIn("authorization_code", payload["authorized_grant_types"])

    def test_existing_client_rotates_secret_without_putting_secret_in_config_body(self):
        calls = []

        def request(_context, url, **kwargs):
            calls.append((url, kwargs))
            if url.endswith("/ssh-proxy"):
                return 200, {"authorized_grant_types": ["authorization_code"], "scope": ["openid", "cloud_controller.read", "cloud_controller.write", "cloud_controller.admin"]}
            if url.endswith("/opensandbox-capi") and "method" not in kwargs:
                if len(calls) == 1:
                    return 200, {"authorized_grant_types": ["client_credentials"], "scope": MODULE.CLIENT_SCOPES}
                return 200, {"authorized_grant_types": ["client_credentials"], "scope": MODULE.CLIENT_SCOPES, "authorities": MODULE.CLIENT_AUTHORITIES}
            if url.endswith("/opensandbox-capi"):
                return 200, {"authorized_grant_types": ["client_credentials"], "scope": MODULE.CLIENT_SCOPES, "authorities": MODULE.CLIENT_AUTHORITIES}
            return 200, {}

        with patch.object(MODULE, "uaa_request", side_effect=request):
            MODULE.ensure_client(object(), "https://uaa.test", "admin-token", "rotated-secret", apply=True)

        self.assertEqual(calls[1][1]["method"], "PUT")
        self.assertNotIn("client_secret", calls[1][1]["payload"])
        secret_call = next(call for call in calls if call[0].endswith("/opensandbox-capi/secret"))
        self.assertEqual(secret_call[1]["payload"]["secret"], "rotated-secret")
        self.assertTrue(any(call[0].endswith("/ssh-proxy") for call in calls))

    def test_dry_run_existing_client_does_not_change_uua_record(self):
        calls = []

        def request(_context, url, **kwargs):
            calls.append((url, kwargs))
            if url.endswith("/ssh-proxy"):
                return 200, {"authorized_grant_types": ["authorization_code"], "scope": ["openid", "cloud_controller.read", "cloud_controller.write", "cloud_controller.admin"]}
            return 200, {"authorized_grant_types": ["client_credentials"], "scope": MODULE.CLIENT_SCOPES}

        with patch.object(MODULE, "uaa_request", side_effect=request):
            MODULE.ensure_client(object(), "https://uaa.test", "admin-token", "existing-secret", apply=False)

        self.assertFalse(any(kwargs.get("method") in {"PUT", "POST", "DELETE"} for _, kwargs in calls))

    def test_oauth_client_has_only_the_configured_capi_authorities(self):
        self.assertEqual(
            MODULE.CLIENT_SCOPES,
            ["openid", "cloud_controller.read", "cloud_controller.write", "cloud_controller.admin"],
        )
        self.assertEqual(MODULE.CLIENT_AUTHORITIES, ["cloud_controller.read", "cloud_controller.write", "cloud_controller.admin"])

    def test_secret_file_mode_is_private(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "client.secret"
            MODULE.save_secret(Path(directory), path.name, "do-not-print")
            self.assertEqual(path.read_text().strip(), "do-not-print")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def test_existing_local_api_key_is_reused(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "api-key.secret"
            path.write_text("kept-key\n")
            os.chmod(path, 0o600)
            self.assertEqual(MODULE.save_secret(Path(directory), path.name, "new-key"), "kept-key")

    def test_dry_run_does_not_rotate_existing_secret(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "client.secret"
            path.write_text("saved-secret\n")
            self.assertEqual(MODULE.save_secret(Path(directory), path.name, "ssh-proxy-secret"), "saved-secret")
            self.assertEqual(path.read_text().strip(), "saved-secret")

    def test_identity_domain_must_report_policy_enforcement(self):
        self.assertTrue(MODULE.verify_identity_domain("apps.identity shared enforced (any)", "apps.identity"))
        self.assertFalse(MODULE.verify_identity_domain("apps.identity shared", "apps.identity"))
        self.assertFalse(MODULE.verify_identity_domain("apps.identity-elsewhere shared enforced", "apps.identity"))

    def test_ssh_proxy_must_remain_authorization_code_only(self):
        with patch.object(
            MODULE,
            "uaa_request",
            return_value=(200, {"authorized_grant_types": ["authorization_code", "refresh_token", "client_credentials"], "scope": ["openid", "cloud_controller.read", "cloud_controller.write", "cloud_controller.admin"]}),
        ):
            with self.assertRaises(SystemExit):
                MODULE.verify_existing_grants(object(), "https://uaa.test/oauth/clients", "admin-token")


if __name__ == "__main__":
    unittest.main()
