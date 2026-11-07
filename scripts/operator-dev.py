#!/usr/bin/env python3
"""Bootstrap named local operators and exercise the real isolated issuer."""
import argparse
import base64
import getpass
import hashlib
import hmac
import importlib
import json
import re
import secrets
import struct
import subprocess
import sys
import time
import uuid

smoke = importlib.import_module("m1-smoke")


def admin(action, **fields):
    completed = subprocess.run(
        ["make", "--no-print-directory", "-s", "operator-admin"],
        input=json.dumps({"action": action, **fields}),
        capture_output=True, text=True, timeout=45, check=False,
    )
    smoke.require(completed.returncode == 0, "Private operator administration failed")
    return completed.stdout.strip()


def create(name, password, role):
    result = admin("create", identity={
        "schema_id": "operator",
        "traits": {"username": name},
        "metadata_public": {"customer_fabric_role": role},
        "credentials": {
            "password": {"config": {"password": password}},
        },
    })
    return str(uuid.UUID(result))


def bootstrap(name):
    smoke.require(sys.stdin.isatty() and sys.stdout.isatty(), "Bootstrap requires an interactive terminal")
    smoke.require(re.fullmatch(r"[a-z0-9][a-z0-9._-]{2,63}", name), "Use a named synthetic username")
    password = getpass.getpass("Synthetic operator password (at least 20 characters): ")
    smoke.require(len(password) >= 20 and password == getpass.getpass("Confirm password: "), "Password does not match policy or confirmation")
    subject = create(name, password, "profile_reader")
    print(f"Operator subject: {subject}")
    print("Sign in with the operator issuer and enroll TOTP through Kratos account settings.")


def totp_code(seed):
    interval = int(time.time()) // 30
    digest = hmac.new(base64.b32decode(seed), struct.pack(">Q", interval), hashlib.sha1).digest()
    offset = digest[-1] & 15
    code = int.from_bytes(digest[offset:offset + 4], "big") & 0x7fffffff
    return f"{code % 1000000:06}"


def token_header(token):
    return {"X-Session-Token": token}


def bearer(token):
    return {"Authorization": "Bearer " + token}


def login_password(kratos, username, password):
    flow = smoke.request(kratos, "GET", "/self-service/login/api", 200, read_json=True)
    session = smoke.request(kratos, "POST", smoke.submit_path("login", flow["id"]), 200, {
        "method": "password", "identifier": username, "password": password,
    }, read_json=True)
    return session["session_token"]


def enroll_totp(kratos, username, password):
    token = login_password(kratos, username, password)
    flow = smoke.request(kratos, "GET", "/self-service/settings/api", 200,
                         headers=token_header(token), read_json=True)
    node = next(node for node in flow["ui"]["nodes"]
                if node["attributes"].get("id") == "totp_secret_key")
    candidates = re.findall(r"\b[A-Z2-7]{16,128}\b", node["attributes"]["text"]["text"])
    smoke.require(len(candidates) == 1, "Kratos did not supply an enrollment seed")
    seed = candidates[0]
    smoke.request(kratos, "POST", smoke.submit_path("settings", flow["id"]), 200, {
        "method": "totp", "totp_code": totp_code(seed),
    }, headers=token_header(token))
    smoke.request(kratos, "DELETE", "/self-service/logout/api", 204, {"session_token": token})
    return seed


def login(kratos, username, password, seed, aal1_check=None):
    token = login_password(kratos, username, password)
    if aal1_check:
        aal1_check(token)
    upgrade = smoke.request(kratos, "GET", "/self-service/login/api?aal=aal2", 200,
                            headers=token_header(token), read_json=True)
    result = smoke.request(kratos, "POST", smoke.submit_path("login", upgrade["id"]), 200, {
        "method": "totp", "totp_code": totp_code(seed),
    }, headers=token_header(token), read_json=True)
    return result["session_token"]


def verify(api, kratos, customer, mail):
    smoke.KRATOS, smoke.MAILPIT = customer, mail
    deadline = time.monotonic() + 30
    while True:
        try:
            smoke.request(kratos, "GET", "/health/ready", 200)
            break
        except smoke.SmokeFailure:
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.5)
    name = "operator-" + uuid.uuid4().hex
    password = secrets.token_urlsafe(32)
    subject = create(name, password, "profile_reader")
    seed = enroll_totp(kratos, name, password)
    source = json.loads(smoke.env_value(".env", "CF_DEV_TOKENS"))["importer"]
    previous = smoke.env_value(".env.m1", "CF_DEV_OPERATOR_TOKEN")
    smoke.request(api, "GET", smoke.OPERATOR, 401, headers=bearer(source))
    smoke.request(api, "GET", smoke.OPERATOR, 401, headers=bearer(previous))
    customer_session = smoke.create_verified_session(name + "@example.test")
    smoke.request(api, "GET", smoke.OPERATOR, 401, headers=token_header(customer_session))
    smoke.request(api, "GET", smoke.OPERATOR, 401, headers=bearer(customer_session))
    token = login(kratos, name, password, seed, lambda value:
                  smoke.request(api, "GET", smoke.OPERATOR, 403, headers=bearer(value)))
    smoke.request(api, "GET", smoke.OPERATOR, 200, headers=bearer(token))
    smoke.request(api, "POST", smoke.OPERATOR, 405, {}, bearer(token))
    smoke.request(api, "POST", smoke.SOURCE, 401, {}, bearer(token))
    smoke.request(api, "GET", smoke.CUSTOMER, 401, headers=token_header(token))

    unassigned = name + "-unassigned"
    unassigned_subject = create(unassigned, password, "")
    unassigned_seed = enroll_totp(kratos, unassigned, password)
    no_role = login(kratos, unassigned, password, unassigned_seed)
    smoke.request(api, "GET", smoke.OPERATOR, 403, headers=bearer(no_role))

    smoke.request(kratos, "DELETE", "/self-service/logout/api", 204, {"session_token": token})
    smoke.request(api, "GET", smoke.OPERATOR, 401, headers=bearer(token))
    restored = login(kratos, name, password, seed)
    smoke.request(api, "GET", smoke.OPERATOR, 200, headers=bearer(restored))
    admin("revoke", id=subject)
    smoke.request(api, "GET", smoke.OPERATOR, 401, headers=bearer(restored))
    admin("disable", id=unassigned_subject)
    smoke.request(api, "GET", smoke.OPERATOR, 401, headers=bearer(no_role))
    smoke.request(customer, "DELETE", "/self-service/logout/api", 204, {"session_token": customer_session})
    print("PASS: named Kratos operator, TOTP step-up, explicit read permission, audience separation, logout and revocation")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    setup = modes.add_parser("bootstrap")
    setup.add_argument("name", nargs="?")
    test = modes.add_parser("smoke")
    for arg in ("api", "kratos", "customer", "mail"):
        test.add_argument(arg, type=smoke.loopback_address)
    for action in ("revoke", "disable"):
        mode = modes.add_parser(action)
        mode.add_argument("subject", type=uuid.UUID)
    args = parser.parse_args()
    if args.mode == "bootstrap":
        bootstrap(args.name or input("Named synthetic username: "))
    elif args.mode in {"revoke", "disable"}:
        subject = str(args.subject)
        if args.mode == "disable":
            admin("disable", id=subject)
        admin("revoke", id=subject)
        print("Operator sessions revoked.")
    else:
        verify(args.api, args.kratos, args.customer, args.mail)


if __name__ == "__main__":
    try:
        main()
    except smoke.SmokeFailure as exc:
        sys.exit(f"FAIL: {exc}")
    except (KeyError, TypeError, ValueError, OSError, subprocess.TimeoutExpired) as exc:
        sys.exit(f"FAIL: could not complete operator operation ({type(exc).__name__})")
