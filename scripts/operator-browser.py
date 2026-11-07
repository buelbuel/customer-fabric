#!/usr/bin/env python3
"""Test native browser CSRF and session cookies against the operator issuer."""
import argparse
import importlib
import os
import re
import secrets
import sys
import uuid

from playwright.sync_api import sync_playwright

sys.dont_write_bytecode = True
operator = importlib.import_module("operator-dev")
browser = importlib.import_module("m1-browser")
smoke = operator.smoke


def login(page, username, password):
    browser.flow(page, "login")
    browser.submit(page, 'button[name="method"][value="password"]', {
        "identifier": username, "password": password,
    })
    status = page.request.get(browser.native.KRATOS + "/sessions/whoami").status
    smoke.require(status in (200, 403), f"Operator password login failed ({status})")


def walk(page, origin):
    username = "operator-browser-" + uuid.uuid4().hex
    password = secrets.token_urlsafe(32)
    subject = operator.create(username, password, "profile_reader")
    browser.native.KRATOS = origin
    profiles = origin + "/account/operator-profiles"
    require = smoke.require
    require(page.goto(profiles).status == 401, "Unauthenticated browser read was allowed")
    require(page.request.get(origin + "/admin/identities").status == 404, "Operator admin route was exposed")

    login_flow = browser.flow(page, "login")
    unsafe = page.request.post(origin + smoke.submit_path("login", login_flow), form={
        "method": "password", "identifier": username, "password": password,
    })
    require(page.request.get(origin + "/sessions/whoami").status == 401,
            "Operator login accepted a missing CSRF token")
    require(not unsafe.ok or "/account/error" in unsafe.url, "Unexpected operator CSRF rejection")
    login(page, username, password)
    require(page.goto(profiles).status == 403, "Password-only browser session could read profiles")

    browser.flow(page, "settings")
    seed = re.findall(r"\b[A-Z2-7]{16,128}\b", page.locator("#totp_secret_key").inner_text())
    require(len(seed) == 1, "Kratos did not render an enrollment secret")
    browser.submit(page, 'button[name="method"][value="totp"]', {"totp_code": operator.totp_code(seed[0])})
    page.goto(origin + "/account/logout")
    page.get_by_role("link", name="Confirm logout").click()
    require(page.goto(profiles).status == 401, "Operator browser logout left its session active")

    login(page, username, password)
    browser.flow(page, "login", "?aal=aal2")
    browser.submit(page, 'button[name="method"][value="totp"]', {"totp_code": operator.totp_code(seed[0])})
    response = page.goto(profiles)
    require(response.status == 200 and response.headers.get("cache-control") == "no-store",
            "MFA browser session could not read profiles with no-store")
    cookie_name = "cf_operator_" + origin.rsplit(":", 1)[1]
    require(any(c["name"] == cookie_name and c["httpOnly"] and c["sameSite"] == "Strict"
                for c in page.context.cookies()), "Operator browser cookie is not protected")
    require(not page.evaluate("Object.keys(localStorage).length"), "Operator browser persisted a credential")
    operator.admin("revoke", id=subject)
    require(page.goto(profiles).status == 401, "Revoked operator browser session remained active")
    print("PASS: browser operator CSRF, TOTP enrollment, MFA, protected cookie, logout and revocation")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("origin", type=smoke.loopback_address)
    args = parser.parse_args()
    with sync_playwright() as p:
        instance = p.chromium.launch(executable_path=os.environ.get("CF_BROWSER_EXECUTABLE"))
        page = instance.new_page()
        page.set_default_timeout(8000)
        walk(page, args.origin)


if __name__ == "__main__":
    try:
        main()
    except (smoke.SmokeFailure, browser.native.SmokeFailure) as exc:
        sys.exit(f"FAIL: {exc}")
    except Exception as exc:
        # Browser automation errors can contain entered credentials or codes.
        trace = exc.__traceback__
        while trace and trace.tb_next:
            trace = trace.tb_next
        line = trace.tb_lineno if trace else 0
        sys.exit(f"FAIL: operator browser check failed ({type(exc).__name__}, line {line})")
