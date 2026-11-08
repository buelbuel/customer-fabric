#!/usr/bin/env python3
"""Exercise expiry, deactivation and fail-closed behavior in the synthetic Compose stack."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import importlib
import json
import secrets
import subprocess
import sys
import urllib.error
import urllib.request
import uuid

sys.dont_write_bytecode = True
smoke = importlib.import_module("m1-smoke")


def run(compose, args, fixture=None):
    result = subprocess.run(
        [*compose, *args], input=fixture, text=True, capture_output=True, check=False, timeout=60,
    )
    smoke.require(result.returncode == 0, "Isolated Compose test command failed")
    return result.stdout.strip()


def change(compose, table, assignment, row):
    # The table and assignment are fixed fixtures. A parsed UUID is the only dynamic SQL value.
    row = uuid.UUID(row)
    sql = f"UPDATE {table} SET {assignment} WHERE id = '{row}' RETURNING 1;\n"
    result = run(compose, ["exec", "-T", "kratos-db", "psql", "-U", "kratos", "-d", "kratos",
                           "-Atq", "-v", "ON_ERROR_STOP=1"], sql)
    smoke.require(result == "1", "Expected to change exactly one synthetic test row")


def session():
    email = f"m1-session-{uuid.uuid4().hex}@example.test"
    token = smoke.create_verified_session(email)
    auth = {"X-Session-Token": token}
    who = smoke.request(smoke.KRATOS, "GET", "/sessions/whoami", 200, headers=auth, read_json=True)
    smoke.request(smoke.API, "PUT", smoke.CUSTOMER, 200, headers=auth)
    return token, auth, who


def competing_settings():
    token, auth, who = session()
    profile = smoke.request(smoke.API, "GET", smoke.CUSTOMER, 200, headers=auth, read_json=True)
    emails = [f"m1-settings-{uuid.uuid4().hex}@example.test" for _ in range(2)]
    flows = [
        smoke.request(smoke.KRATOS, "GET", "/self-service/settings/api", 200,
                      headers=auth, read_json=True)["id"] for _ in emails
    ]

    def update(flow_id, email):
        # A successful Kratos profile update can invalidate the other outstanding
        # settings flow. A racing submission can then legitimately return 404.
        # Never retry it: require a successful winner and verify the final state.
        request = urllib.request.Request(
            smoke.KRATOS + smoke.submit_path("settings", flow_id),
            data=json.dumps({"method": "profile", "traits": {"email": email}}).encode(),
            headers={"Accept": "application/json", "Content-Type": "application/json", **auth},
            method="POST",
        )
        try:
            response = smoke.HTTP.open(request, timeout=10)
        except urllib.error.HTTPError as exc:
            response = exc
        except (urllib.error.URLError, TimeoutError):
            raise smoke.SmokeFailure("Concurrent settings request could not connect") from None
        with response:
            status = response.status
            response.read()
        smoke.require(status in (200, 404),
                      f"Concurrent settings request returned unexpected HTTP {status}")
        return status

    with ThreadPoolExecutor(max_workers=2) as workers:
        changes = [workers.submit(update, flow_id, email) for flow_id, email in zip(flows, emails)]
        statuses = [change_result.result() for change_result in changes]
    smoke.require(200 in statuses, "Neither concurrent settings update succeeded")

    current = smoke.request(smoke.KRATOS, "GET", "/sessions/whoami", 200,
                            headers=auth, read_json=True)["identity"]
    email = current["traits"]["email"]
    smoke.require(current["id"] == who["identity"]["id"] and email in emails,
                  "Concurrent settings updates lost the original subject or both changes")
    smoke.request(smoke.API, "GET", smoke.CUSTOMER, 403, headers=auth)
    flow = smoke.request(smoke.KRATOS, "GET", "/self-service/verification/api", 200, read_json=True)
    path = smoke.submit_path("verification", flow["id"])
    smoke.request(smoke.KRATOS, "POST", path, 200, {"method": "code", "email": email})
    code = smoke.verification_code(email, flow["id"])
    smoke.request(smoke.KRATOS, "POST", path, 200, {"method": "code", "code": code})
    verified = smoke.request(smoke.API, "GET", smoke.CUSTOMER, 200, headers=auth, read_json=True)
    smoke.require(verified == profile, "Concurrent settings updates changed the profile anchor")
    smoke.request(smoke.KRATOS, "DELETE", "/self-service/logout/api", 204, {"session_token": token})
    print("PASS: concurrent email settings kept the profile anchored and required current-email verification")


def verify(compose):
    _, auth, who = session()
    settings = smoke.request(smoke.KRATOS, "GET", "/self-service/settings/api", 200,
                             headers=auth, read_json=True)
    for kind in ("verification", "recovery", "settings"):
        if kind == "settings":
            flow = settings
            headers = auth
        else:
            flow = smoke.request(smoke.KRATOS, "GET", f"/self-service/{kind}/api", 200, read_json=True)
            headers = None
        change(compose, f"selfservice_{kind}_flows", "expires_at = now() - interval '1 minute'", flow["id"])
        # An expired flow cannot be read as an active flow. Check both the
        # persisted expiry and the actual Kratos lookup before testing session denial.
        smoke.request(smoke.KRATOS, "GET", f"/self-service/{kind}/flows?id={flow['id']}",
                      410, headers=headers)
    change(compose, "sessions", "authenticated_at = now() - interval '6 minutes'", who["id"])
    smoke.request(smoke.API, "GET", smoke.CUSTOMER, 200, headers=auth)
    stale_settings = smoke.request(smoke.KRATOS, "GET", "/self-service/settings/api", 200,
                                  headers=auth, read_json=True)
    smoke.request(smoke.KRATOS, "POST", smoke.submit_path("settings", stale_settings["id"]), 403,
                  {"method": "password", "password": secrets.token_urlsafe(32)}, auth)
    change(compose, "sessions", "expires_at = now() - interval '1 minute'", who["id"])
    smoke.request(smoke.API, "GET", smoke.CUSTOMER, 401, headers=auth)

    _, auth, who = session()
    change(compose, "identities", "state = 'inactive'", who["identity"]["id"])
    smoke.request(smoke.API, "GET", smoke.CUSTOMER, 401, headers=auth)

    token, auth, _ = session()
    try:
        run(compose, ["stop", "kratos"])
        smoke.request(smoke.API, "GET", smoke.CUSTOMER, 503, headers=auth)
    finally:
        run(compose, ["start", "kratos"])
        smoke.wait_ready()
    smoke.request(smoke.API, "GET", smoke.CUSTOMER, 200, headers=auth)
    smoke.request(smoke.KRATOS, "DELETE", "/self-service/logout/api", 204, {"session_token": token})
    print("PASS: real Kratos expired flows and session, inactive identity and fail-closed outage")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for address in ("api", "kratos", "mailpit"):
        parser.add_argument("--" + address, type=smoke.loopback_address, required=True)
    parser.add_argument("compose", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    smoke.API, smoke.KRATOS, smoke.MAILPIT = args.api, args.kratos, args.mailpit
    smoke.require(args.compose[:3] == ["--", "docker", "compose"], "Expected the scoped Compose command")
    verify(args.compose[1:])
    competing_settings()


if __name__ == "__main__":
    try:
        main()
    except (smoke.SmokeFailure, OSError, KeyError, ValueError, subprocess.TimeoutExpired) as exc:
        message = str(exc) if isinstance(exc, smoke.SmokeFailure) else type(exc).__name__
        sys.exit(f"FAIL: customer session smoke ({message})")
