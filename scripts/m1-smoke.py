#!/usr/bin/env python3
"""Exercise native Kratos flows and the local M1 API using synthetic data."""
import argparse
import json
import re
import secrets
import shlex
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
API = "http://127.0.0.1:8080"
KRATOS = "http://127.0.0.1:4433"
MAILPIT = "http://127.0.0.1:8025"
CUSTOMER = "/v1/customer/profile"
OPERATOR = "/v1/operator/profiles"
SOURCE = "/v1/profiles"


class SmokeFailure(Exception):
    """An assertion failure whose message contains no credentials or payloads."""


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def require(condition, message):
    if not condition:
        raise SmokeFailure(message)


def env_value(filename, key):
    for line in (ROOT / filename).read_text().splitlines():
        name, sep, value = line.partition("=")
        if sep and name.strip() == key:
            parts = shlex.split(value)
            require(len(parts) == 1, f"Invalid value for {key} in {filename}")
            return parts[0]
    raise SmokeFailure(f"Missing {key} in {filename}. Run make m1-up.")


def request(base, method, path, status, payload=None, headers=None, read_json=False, timeout=10):
    request_headers = {"Accept": "application/json", **(headers or {})}
    data = None
    if payload is not None:
        data = json.dumps(payload).encode()
        request_headers["Content-Type"] = "application/json"
    req = urllib.request.Request(base + path, data=data, headers=request_headers, method=method)
    try:
        response = HTTP.open(req, timeout=timeout)
    except urllib.error.HTTPError as exc:
        response = exc
    except (urllib.error.URLError, TimeoutError):
        raise SmokeFailure(f"{method} {path.split('?')[0]} could not connect to {base}") from None
    with response:
        require(response.status == status,
                f"{method} {path.split('?')[0]} returned {response.status}, expected {status}")
        if read_json:
            return json.load(response)


def native_flow(kind):
    flow = request(KRATOS, "GET", f"/self-service/{kind}/api", 200, read_json=True)
    return submit_path(kind, flow["id"])


def submit_path(kind, flow_id):
    return f"/self-service/{kind}?flow={uuid.UUID(flow_id)}"


def verification_code(email, flow_id, previous_code=None):
    deadline = time.monotonic() + 30
    checked = set()
    while time.monotonic() < deadline:
        inbox = request(MAILPIT, "GET", "/api/v1/messages?limit=50", 200, read_json=True)
        for message in inbox.get("messages", []):
            if message["ID"] in checked:
                continue
            if not any(address["Address"] == email for address in message["To"]):
                continue
            checked.add(message["ID"])
            message_id = urllib.parse.quote(message["ID"], safe="")
            detail = request(MAILPIT, "GET", f"/api/v1/message/{message_id}", 200, read_json=True)
            # Registration and blocked login can send mail for different verification flows.
            for link in re.findall(r"https?://\S+", detail["Text"]):
                url = urllib.parse.urlsplit(link)
                query = urllib.parse.parse_qs(url.query)
                if (f"{url.scheme}://{url.netloc}" != KRATOS
                        or url.path != "/self-service/verification"
                        or query.get("flow") != [flow_id]):
                    continue
                codes = query.get("code", [])
                require(len(codes) == 1 and re.fullmatch(r"[0-9]{6}", codes[0]),
                        "Verification mail did not contain a six-digit code")
                if codes[0] != previous_code:
                    return codes[0]
        time.sleep(0.5)
    raise SmokeFailure("Timed out waiting for the synthetic verification mail")


def create_verified_session(email):
    password = secrets.token_urlsafe(32)
    registration = request(KRATOS, "POST", native_flow("registration"), 200, {
        "method": "password", "password": password, "traits": {"email": email},
    }, read_json=True)
    identity_id = registration["identity"]["id"]
    require(not registration.get("session_token") and not registration.get("session"),
            "Registration issued a session before email verification")

    login = {"method": "password", "identifier": email, "password": password}
    blocked = request(KRATOS, "POST", native_flow("login"), 403, login, read_json=True)
    continuations = blocked["error"]["details"]["continue_with"]
    verification = next((step for step in continuations if step["action"] == "show_verification_ui"), None)
    require(verification is not None, "Unverified login did not start verification")
    flow_id = verification["flow"]["id"]
    result = request(KRATOS, "POST", submit_path("verification", flow_id), 200, {
        "method": "code", "code": verification_code(email, flow_id),
    }, read_json=True)
    require(result["state"] == "passed_challenge", "Kratos did not verify the email code")

    authenticated = request(KRATOS, "POST", native_flow("login"), 200, login, read_json=True)
    token = authenticated.get("session_token")
    require(isinstance(token, str) and bool(token), "Verified login did not issue a session token")
    session = request(KRATOS, "GET", "/sessions/whoami", 200,
                      headers={"X-Session-Token": token}, read_json=True)
    require(session["active"] is True and session["identity"]["id"] == identity_id,
            "Kratos session did not belong to the registered identity")
    require(any(address["verified"] is True and address["value"] == email
                for address in session["identity"]["verifiable_addresses"]),
            "Kratos session did not have the verified email address")
    return token


def wait_ready():
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            request(KRATOS, "GET", "/health/ready", 200, timeout=2)
            request(API, "GET", "/healthz", 200, timeout=2)
            return
        except SmokeFailure:
            time.sleep(0.5)
    raise SmokeFailure("The M1 API and Kratos did not both become ready within 30 seconds")


def loopback_address(value):
    host, _, port = value.partition(":")
    if host != "127.0.0.1" or not port.isascii() or not port.isdecimal() or not 1 <= int(port) <= 65535:
        raise argparse.ArgumentTypeError("expected a published IPv4 loopback address")
    return f"http://127.0.0.1:{int(port)}"


def main():
    global API, KRATOS, MAILPIT
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ready", action="store_true", help="Only wait for the local services")
    parser.add_argument("--api", type=loopback_address)
    parser.add_argument("--kratos", type=loopback_address)
    parser.add_argument("--mailpit", type=loopback_address)
    args = parser.parse_args()
    API = args.api or API
    KRATOS = args.kratos or KRATOS
    MAILPIT = args.mailpit or MAILPIT
    wait_ready()
    if args.ready:
        print("PASS: local M1 API and Kratos ready")
        return

    source_tokens = json.loads(env_value(".env", "CF_DEV_TOKENS"))
    operator_token = env_value(".env.m1", "CF_DEV_OPERATOR_TOKEN")
    source_auth = {"Authorization": "Bearer " + source_tokens["importer"]}
    other_source_auth = {"Authorization": "Bearer " + source_tokens["console"]}
    operator_auth = {"Authorization": "Bearer " + operator_token}

    request(API, "GET", CUSTOMER, 401)
    request(API, "PUT", CUSTOMER, 401)
    request(API, "GET", CUSTOMER, 401, headers={"X-Session-Token": secrets.token_urlsafe(32)})
    request(API, "GET", CUSTOMER, 401, headers=source_auth)
    request(API, "GET", CUSTOMER, 401, headers=operator_auth)
    request(API, "GET", OPERATOR, 401)
    request(API, "GET", OPERATOR, 401, headers=source_auth)
    request(API, "GET", OPERATOR, 200, headers=operator_auth)
    request(API, "GET", SOURCE, 403, headers=source_auth)

    suffix = uuid.uuid4().hex
    email = f"m1-smoke-{suffix}@example.test"
    crm = request(API, "POST", SOURCE, 201, {
        "set": {"display_name": "Synthetic CRM record", "email": email},
        "identifiers": [{"namespace": "crm", "value": "m1-smoke-" + suffix}],
    }, {**source_auth, "Idempotency-Key": "m1-smoke-" + suffix}, read_json=True)
    crm_path = OPERATOR + "/" + str(uuid.UUID(crm["id"]))
    crm_before = request(API, "GET", crm_path, 200, headers=operator_auth, read_json=True)
    require(crm_before["attributes"]["email"] == email, "Synthetic CRM profile did not retain its email")
    request(API, "GET", SOURCE + "/" + crm["id"], 403, headers=source_auth)

    token = create_verified_session(email)
    customer_auth = {"X-Session-Token": token}
    request(API, "GET", OPERATOR, 401, headers=customer_auth)
    request(API, "POST", SOURCE, 401, {}, customer_auth)
    request(API, "GET", CUSTOMER, 401, headers={"Authorization": "Bearer " + token})
    request(API, "GET", CUSTOMER + "/" + crm["id"], 404, headers=customer_auth)
    request(API, "PUT", CUSTOMER, 400, {"id": crm["id"]}, customer_auth)
    request(API, "GET", CUSTOMER, 404, headers=customer_auth)
    profile = request(API, "PUT", CUSTOMER, 200, headers=customer_auth, read_json=True)
    require(set(profile) == {"id", "version", "attributes"}, "Customer response exposed unexpected fields")
    require(profile["id"] != crm["id"] and profile["attributes"] == {} and profile["version"] == 1,
            "Provisioning failed to create a separate empty customer profile")
    for method in ("PUT", "GET"):
        repeated = request(API, method, CUSTOMER, 200, headers=customer_auth, read_json=True)
        require(repeated == profile, "Customer profile changed during idempotent provisioning or lookup")

    mutation = {"set": {"display_name": "Updated synthetic CRM record"}}
    patch_headers = {"If-Match": '"1"'}
    request(API, "PATCH", SOURCE + "/" + profile["id"], 403, mutation,
            {**source_auth, **patch_headers, "Idempotency-Key": "m1-customer-denied-" + suffix})
    request(API, "PATCH", SOURCE + "/" + crm["id"], 403, mutation,
            {**other_source_auth, **patch_headers, "Idempotency-Key": "m1-cross-source-" + suffix})
    customer_after = request(API, "GET", CUSTOMER, 200, headers=customer_auth, read_json=True)
    require(customer_after == profile, "A denied source mutation changed the customer profile")
    crm_after = request(API, "GET", crm_path, 200, headers=operator_auth, read_json=True)
    require(crm_after == crm_before, "Provisioning or a denied mutation modified the same-email CRM profile")
    request(API, "PATCH", SOURCE + "/" + crm["id"], 200, mutation,
            {**source_auth, **patch_headers, "Idempotency-Key": "m1-own-source-" + suffix})

    request(KRATOS, "DELETE", "/self-service/logout/api", 204, {"session_token": token})
    request(KRATOS, "GET", "/sessions/whoami", 401, headers=customer_auth)
    request(API, "GET", CUSTOMER, 401, headers=customer_auth)
    print("PASS: real Kratos registration, email code verification, login and logout")
    print("PASS: customer, operator and source authentication separation")
    print("PASS: idempotent customer provisioning kept the same-email CRM profile separate")


if __name__ == "__main__":
    try:
        main()
    except SmokeFailure as exc:
        sys.exit(f"FAIL: {exc}")
    except (KeyError, TypeError, ValueError, OSError) as exc:
        sys.exit(f"FAIL: could not complete M1 smoke test ({type(exc).__name__})")
