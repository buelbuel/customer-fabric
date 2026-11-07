#!/usr/bin/env python3
"""Exercise expiry, deactivation and fail-closed behavior in the synthetic Compose stack."""
import argparse
import importlib
import subprocess
import sys
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


if __name__ == "__main__":
    try:
        main()
    except (smoke.SmokeFailure, OSError, KeyError, ValueError, subprocess.TimeoutExpired) as exc:
        message = str(exc) if isinstance(exc, smoke.SmokeFailure) else type(exc).__name__
        sys.exit(f"FAIL: customer session smoke ({message})")
