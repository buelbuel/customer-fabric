#!/usr/bin/env python3
"""Exercise expiry, deactivation and fail-closed behavior in the synthetic Compose stack."""
import argparse
import collections
from concurrent.futures import ThreadPoolExecutor, as_completed
import importlib
import json
from pathlib import Path
import secrets
import subprocess
import sys
import threading
import urllib.error
import urllib.request
import uuid

sys.dont_write_bytecode = True
smoke = importlib.import_module("m1-smoke")
LOST_ROW_MESSAGE = "Unable to locate the resource"
LOST_ADDRESS_CONSTRAINT = "identity_verification_codes_identity_verifiable_addresses_id_fk"


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


def racing_settings_update(barrier, flow_id, email, auth, subject):
    """Submit one settings update and accept only the outcomes observed from pinned Kratos v26.2.0."""
    request = urllib.request.Request(
        smoke.KRATOS + smoke.submit_path("settings", flow_id),
        data=json.dumps({"method": "profile", "traits": {"email": email}}).encode(),
        headers={"Accept": "application/json", "Content-Type": "application/json", **auth},
        method="POST",
    )
    barrier.wait(timeout=10)
    try:
        response = smoke.HTTP.open(request, timeout=15)
    except urllib.error.HTTPError as exc:
        response = exc
    except (urllib.error.URLError, TimeoutError):
        raise smoke.SmokeFailure("Concurrent settings request could not connect") from None
    with response:
        status = response.status
        body = json.load(response)
    if status == 200:
        smoke.require(body["identity"]["id"] == subject, "A settings update returned another subject")
        return status
    # Kratos diffs against an identity read outside its write transaction, so the
    # loser can fail on a row the winner replaced, sometimes after its own write
    # committed. A flow that really disappeared has a different reason and fails here.
    error = body.get("error", {})
    message = error.get("message", "")
    lost_row = status == 404 and error.get("code") == 404 and message == LOST_ROW_MESSAGE
    lost_address = status == 400 and error.get("code") == 500 and LOST_ADDRESS_CONSTRAINT in message
    smoke.require(lost_row or lost_address, f"Concurrent settings request returned unexpected HTTP {status}")
    return status


def verify_address(email):
    flow = smoke.request(smoke.KRATOS, "GET", "/self-service/verification/api", 200, read_json=True)
    path = smoke.submit_path("verification", flow["id"])
    smoke.request(smoke.KRATOS, "POST", path, 200, {"method": "code", "email": email})
    code = smoke.verification_code(email, flow["id"])
    smoke.request(smoke.KRATOS, "POST", path, 200, {"method": "code", "code": code})


def competing_settings(bystander):
    token, auth, who = session()
    subject = who["identity"]["id"]
    original = who["identity"]["traits"]["email"]
    profile = smoke.request(smoke.API, "GET", smoke.CUSTOMER, 200, headers=auth, read_json=True)
    emails = [f"m1-settings-{uuid.uuid4().hex}@example.test" for _ in range(2)]
    flows = [
        smoke.request(smoke.KRATOS, "GET", "/self-service/settings/api", 200,
                      headers=auth, read_json=True)["id"] for _ in emails
    ]

    barrier = threading.Barrier(2)
    with ThreadPoolExecutor(max_workers=2) as workers:
        changes = [workers.submit(racing_settings_update, barrier, flow_id, email, auth, subject)
                   for flow_id, email in zip(flows, emails)]
        statuses = [change_result.result() for change_result in changes]
    winners = [email for email, status in zip(emails, statuses) if status == 200]
    smoke.require(winners, "Neither concurrent settings update succeeded")

    current = smoke.request(smoke.KRATOS, "GET", "/sessions/whoami", 200,
                            headers=auth, read_json=True)["identity"]
    email = current["traits"]["email"]
    smoke.require(current["id"] == subject and email in emails,
                  "Concurrent settings updates lost the subject or both changes")
    addresses = {address["value"]: address["verified"] for address in current["verifiable_addresses"]}
    recovery = {address["value"] for address in current["recovery_addresses"]}
    smoke.require(email in addresses and email in recovery,
                  "Concurrent settings updates did not enroll the current address")
    smoke.require(original not in addresses and original not in recovery,
                  "Concurrent settings updates retained the replaced address")
    smoke.require(set(addresses) <= set(emails) and recovery <= set(emails),
                  "Concurrent settings updates enrolled an address that was never submitted")
    smoke.require(not any(addresses.values()), "A changed address was verified without a code")
    smoke.request(smoke.API, "GET", smoke.CUSTOMER, 403, headers=auth)

    # The pinned Kratos can keep the other submitted address. See docs/development/customer-identity.md.
    for address in sorted(set(addresses) - {email}):
        verify_address(address)
        smoke.request(smoke.API, "GET", smoke.CUSTOMER, 403, headers=auth)

    verify_address(email)
    verified = smoke.request(smoke.API, "GET", smoke.CUSTOMER, 200, headers=auth, read_json=True)
    smoke.require(verified == profile, "Concurrent settings updates changed the profile anchor")

    bystander_auth, bystander_profile = bystander
    other = smoke.request(smoke.API, "GET", smoke.CUSTOMER, 200, headers=bystander_auth, read_json=True)
    smoke.require(other == bystander_profile and other["id"] != profile["id"],
                  "Concurrent settings updates crossed customer accounts")
    smoke.request(smoke.KRATOS, "DELETE", "/self-service/logout/api", 204, {"session_token": token})
    retained = bool((set(addresses) | recovery) - {email})
    return ",".join(str(status) for status in sorted(statuses)), retained, email not in winners


def race_settings(iterations, workers, report):
    _, bystander_auth, _ = session()
    bystander = (bystander_auth,
                 smoke.request(smoke.API, "GET", smoke.CUSTOMER, 200, headers=bystander_auth, read_json=True))
    # Parallel races add the database contention that exposes the losing-writer failures.
    outcomes, failure = [], None
    with ThreadPoolExecutor(max_workers=workers) as pool:
        pending = [pool.submit(competing_settings, bystander) for _ in range(iterations)]
        for finished in as_completed(pending):
            try:
                outcomes.append(finished.result())
            except Exception as exc:
                failure = exc
                for waiting in pending:
                    waiting.cancel()
                break
    retained = sum(outcome[1] for outcome in outcomes)
    summary = {"iterations": iterations, "workers": workers, "completed": len(outcomes), "failed": failure is not None,
               "status_pairs": dict(sorted(collections.Counter(outcome[0] for outcome in outcomes).items())),
               "iterations_retaining_stale_address": retained,
               "iterations_final_email_from_failed_request": sum(outcome[2] for outcome in outcomes)}
    if report:
        report.parent.mkdir(parents=True, exist_ok=True)
        report.write_text(json.dumps(summary, indent=2) + "\n")
    if failure:
        raise failure
    print(f"PASS: concurrent email settings kept the profile anchored and required current-email verification {summary}")
    if retained:
        print(f"KNOWN RISK: Kratos retained a stale address in {retained}/{iterations} races; "
              "this blocks production and is not a passing security property")


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
    parser.add_argument("--settings-race-iterations", type=int, help="Only run the settings race, this many times")
    parser.add_argument("--settings-race-workers", type=int, default=1, help="Run this many races at once")
    parser.add_argument("--settings-race-report", type=Path, help="Write the aggregate race outcomes to this file")
    parser.add_argument("compose", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    smoke.API, smoke.KRATOS, smoke.MAILPIT = args.api, args.kratos, args.mailpit
    smoke.require(args.compose[:3] == ["--", "docker", "compose"], "Expected the scoped Compose command")
    if args.settings_race_iterations is not None:
        smoke.require(args.settings_race_iterations > 0 and args.settings_race_workers > 0,
                      "Expected positive race iteration and worker counts")
        race_settings(args.settings_race_iterations, args.settings_race_workers, args.settings_race_report)
        return
    verify(args.compose[1:])
    race_settings(1, 1, None)


if __name__ == "__main__":
    try:
        main()
    except (smoke.SmokeFailure, OSError, KeyError, ValueError, AttributeError, TypeError,
            threading.BrokenBarrierError, subprocess.TimeoutExpired) as exc:
        message = str(exc) if isinstance(exc, smoke.SmokeFailure) else type(exc).__name__
        sys.exit(f"FAIL: customer session smoke ({message})")
