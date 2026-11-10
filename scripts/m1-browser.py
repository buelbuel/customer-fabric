#!/usr/bin/env python3
"""Exercise real Kratos browser flows with synthetic credentials and captured mail."""
import argparse
import importlib.util
import json
import os
import re
import secrets
import sys
import time
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import sync_playwright

spec = importlib.util.spec_from_file_location("native", Path(__file__).with_name("m1-smoke.py"))
sys.dont_write_bytecode = True
native = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native)


def mail_code(email, flow_id, kind):
    if kind == "verification":
        return native.verification_code(email, flow_id)
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        inbox = native.request(native.MAILPIT, "GET", "/api/v1/messages?limit=50", 200, read_json=True)
        for message in inbox.get("messages", []):
            if not any(address["Address"] == email for address in message["To"]):
                continue
            if "recover" not in message["Subject"].lower():
                continue
            detail = native.request(native.MAILPIT, "GET", "/api/v1/message/" + message["ID"], 200, read_json=True)
            match = re.search(r"(?<![0-9])[0-9]{6}(?![0-9])", detail["Text"])
            if match:
                return match.group()
        time.sleep(0.5)
    raise native.SmokeFailure("No recovery code reached the synthetic mail sink")


def flow(page, kind, query=""):
    page.goto(native.KRATOS + f"/self-service/{kind}/browser" + query)
    page.locator("form").first.wait_for()
    return parse_qs(urlsplit(page.url).query)["flow"][0]


def submit(page, selector, fields):
    form = page.locator("form").filter(has=page.locator(selector)).first
    for name, value in fields.items():
        form.locator(f'input[name="{name}"]').fill(value)
    form.locator(selector).click()


def verify(page, email, invalid=False):
    flow_id = flow(page, "verification")
    method = 'button[name="method"][value="code"]'
    submit(page, method, {"email": email})
    code = mail_code(email, flow_id, "verification")
    if invalid:
        previous_code = code
        page.get_by_role("button", name="Resend code").click()
        flow_id = parse_qs(urlsplit(page.url).query)["flow"][0]
        code = native.verification_code(email, flow_id, previous_code=previous_code)
        submit(page, method, {"code": previous_code})
        native.require(page.locator('input[name="code"]').count() == 1,
                       "A superseded verification code advanced the flow")
    submit(page, method, {"code": code})
    native.request(native.KRATOS, "POST", native.submit_path("verification", flow_id),
                   410, {"method": "code", "code": code})


def login(page, email, password, refresh=False):
    flow(page, "login", "?refresh=true" if refresh else "")
    submit(page, 'button[name="method"][value="password"]', {
        "identifier": email, "password": password,
    })
    native.require(page.request.get(native.KRATOS + "/sessions/whoami").ok,
                   "Browser login failed")


def profile(page, status=200):
    result = page.goto(native.KRATOS + "/account/profile")
    native.require(result.status == status and result.headers.get("cache-control") == "no-store",
                   "Unexpected profile status or cache policy")
    if status == 200 and page.locator("pre").count():
        return json.loads(page.locator("pre").inner_text())


def seed_consent_policy(operator):
    """Publish one synthetic consent purpose through the operator API, and return its ID."""
    purpose = native.request(native.API, "POST", "/v1/operator/consent/purposes", 201, {
        "name": "browser_" + secrets.token_hex(12),
    }, operator, read_json=True)
    policy = native.request(native.API, "POST", f"/v1/operator/consent/policies/{purpose['id']}", 201, {
        "lawful_basis": "consent", "notice_reference": "browser-notice", "explanation": "Synthetic browser explanation.",
        "jurisdiction": "DE", "locale": "de-DE", "channel": "email", "retention_reference": "browser-retention",
    }, operator, read_json=True)
    native.request(native.API, "POST", f"/v1/operator/consent/policy-versions/{policy['id']}/state", 200,
                   {"state": "active"}, operator)
    return purpose


def consent_journey(page, profile_id):
    origin = native.KRATOS
    operator = {"Authorization": "Bearer " + native.env_value(".env.m1", "CF_DEV_OPERATOR_TOKEN")}
    purpose = seed_consent_policy(operator)

    def outcome():
        path = f"/v1/operator/consent/effective/{profile_id}/{purpose['id']}"
        return native.request(native.API, "GET", path, 200, headers=operator, read_json=True)["outcome"]

    def choose(label):
        page.goto(origin + "/account/consent")
        section = page.locator("section").filter(has=page.get_by_role("heading", name=purpose["name"]))
        native.require("Synthetic browser explanation." in section.inner_text(), "Consent page omitted the explanation")
        with page.expect_response(lambda response: response.request.method == "POST"
                                  and urlsplit(response.url).path == "/account/consent") as answer:
            section.get_by_role("button", name=f"{label} {purpose['name']}").click()
        native.require(answer.value.status == 303, f"Consent choice returned {answer.value.status}")

    native.require(outcome() == "blocked_fail_closed", "A purpose was permitted before any customer choice")
    native.require(page.request.post(origin + "/account/consent", headers={"Origin": "https://example.test"}).status == 403,
                   "Cross-origin consent change was accepted")
    choose("Allow")
    native.require(outcome() == "permitted", "A browser grant did not permit processing")
    choose("Withdraw")
    native.require(outcome() == "blocked", "A browser withdrawal did not stop processing")
    rows = page.locator("table tbody tr").filter(has_text=purpose["name"])
    rows.nth(1).wait_for()
    native.require(rows.count() == 2 and "withdraw" in rows.first.inner_text(),
                   "Decision history did not show the audited changes")
    print("PASS: browser consent notice, grant, withdrawal, history and cross-origin rejection", flush=True)


def preferences_journey(page):
    origin = native.KRATOS
    page.goto(origin + "/account/preferences")
    page.get_by_label("Display name").fill("  Synthetic Zoë ")
    page.get_by_label("Preferred language").fill("de-de")
    page.get_by_label("Time zone").fill("Europe/Berlin")
    with page.expect_response(lambda response: response.request.method == "POST"
                              and urlsplit(response.url).path == "/account/preferences") as answer:
        page.get_by_role("button", name="Save preferences").click()
    native.require(answer.value.status == 303, f"Saving preferences returned {answer.value.status}")
    native.require(page.get_by_label("Display name").input_value() == "Synthetic Zoë"
                   and page.get_by_label("Preferred language").input_value() == "de-DE",
                   "Saved preferences were not normalized and shown")
    page.get_by_label("Time zone").fill("Mars/Olympus")
    page.get_by_role("button", name="Save preferences").click()
    native.require("Check the values" in page.get_by_role("status").inner_text()
                   and page.get_by_label("Time zone").input_value() == "Mars/Olympus",
                   "An invalid time zone was not refused with the typed value kept")
    native.require(page.request.post(origin + "/account/preferences", headers={"Origin": "https://example.test"}).status == 403,
                   "Cross-origin preference change was accepted")
    print("PASS: browser preferences edit, normalization, validation and cross-origin rejection", flush=True)


def preferences_persisted(page):
    page.goto(native.KRATOS + "/account/preferences")
    native.require(page.get_by_label("Display name").input_value() == "Synthetic Zoë"
                   and page.get_by_label("Time zone").input_value() == "Europe/Berlin",
                   "Preferences did not survive logging in again")
    print("PASS: preferences read back after logging in again", flush=True)


def walk(browser):
    page = browser.new_page()
    page.set_default_timeout(8000)
    origin = native.KRATOS
    email = f"m1-browser-{secrets.token_hex(12)}@example.test"
    new_email = f"m1-browser-{secrets.token_hex(12)}@example.test"
    password, updated, recovered = (secrets.token_urlsafe(32) for _ in range(3))

    profile(page, 401)
    denied_settings = page.request.get(origin + "/self-service/settings/browser", max_redirects=0)
    native.require(denied_settings.status in (302, 303, 401, 403)
                   and "/account/settings" not in denied_settings.headers.get("location", ""),
                   "Unauthenticated browser obtained a settings flow")
    native.require(page.request.get(origin + "/admin/identities").status == 404, "Admin route exposed")
    native.require(page.request.get(origin + "/account/login?flow=invalid").status == 400,
                   "Invalid browser flow was rendered")
    unsafe_return = page.request.get(
        origin + "/self-service/login/browser?return_to=https%3A%2F%2Fexample.test", max_redirects=0
    )
    destination = unsafe_return.headers.get("location", "")
    native.require((unsafe_return.status >= 400 and not destination) or destination.startswith(origin + "/")
                   or (destination.startswith("/") and not destination.startswith("//")),
                   "Untrusted return URL escaped the configured origin")
    flow_id = flow(page, "registration")
    unsafe = page.request.post(origin + native.submit_path("registration", flow_id),
                               form={"method": "password", "traits.email": email, "password": password})
    native.require(not unsafe.ok or "/account/error" in unsafe.url, "Registration accepted a missing CSRF token")
    flow(page, "registration")
    submit(page, 'button[name="method"][value="profile"]', {"traits.email": email})
    submit(page, 'button[name="method"][value="password"]', {"password": password})
    profile(page, 401)
    verify(page, email, invalid=True)
    login(page, email, password)

    cookies = page.context.cookies()
    native.require(any(c["name"].startswith("cf_customer_") and c["httpOnly"]
                       and c["sameSite"] == "Lax" for c in cookies), "Browser session cookie is not protected")
    native.require(not page.evaluate("Object.keys(localStorage).length"), "Browser persisted session data")
    native.require(page.request.post(origin + "/account/profile", headers={"Origin": "https://example.test"}).status == 403,
                   "Cross-origin profile provisioning was accepted")
    profile(page)
    with page.expect_response(lambda response: response.request.method == "POST"
                              and urlsplit(response.url).path == "/account/profile") as provision:
        page.get_by_role("button", name="Create my profile").click()
    native.require(provision.value.status == 303,
                   f"Browser profile provisioning returned {provision.value.status}")
    original = profile(page)
    native.require(original is not None and original["attributes"] == {}, "Browser profile was not provisioned")
    print("PASS: browser registration, CSRF, verification resend, login and own-profile access", flush=True)
    consent_journey(page, original["id"])
    preferences_journey(page)

    flow(page, "settings")
    submit(page, 'button[name="method"][value="profile"]', {"traits.email": new_email})
    profile(page, 403)
    verify(page, new_email)
    native.require(profile(page) == original, "Email change moved the identity to a different profile")

    flow(page, "settings")
    submit(page, 'button[name="method"][value="password"]', {"password": updated})
    second = browser.new_page()
    login(second, new_email, updated)
    native.require(profile(second) == original, "Password change moved the identity to a different profile")
    preferences_persisted(second)
    native.require(profile(page) == original, "Expected the pre-recovery browser session to be active")
    print("PASS: browser settings, re-verification access gate and password change", flush=True)

    recovery = browser.new_page()
    recovery_id = flow(recovery, "recovery")
    method = 'button[name="method"][value="code"]'
    submit(recovery, method, {"email": new_email})
    code = mail_code(new_email, recovery_id, "recovery")
    wrong = "000000" if code != "000000" else "111111"
    submit(recovery, method, {"code": wrong})
    native.require(recovery.locator('input[name="code"]').count() == 1,
                   "An incorrect recovery code advanced the flow")
    # Replay from the original, still unauthenticated browser state.
    replay = browser.new_context()
    replay.add_cookies(recovery.context.cookies())
    csrf = recovery.locator('input[name="csrf_token"]').first.input_value()
    submit(recovery, method, {"code": code})
    recovery.locator('input[name="password"]').wait_for()
    profile(page, 401)
    profile(second, 401)
    submit(recovery, 'button[name="method"][value="password"]', {"password": recovered})
    native.require(profile(recovery) == original, "Recovery moved the identity to a different profile")
    recovery.goto(origin + "/account/logout")
    recovery.get_by_role("link", name="Confirm logout").click()
    profile(recovery, 401)
    replay.request.post(origin + native.submit_path("recovery", recovery_id), form={
        "method": "code", "code": code, "csrf_token": csrf,
    })
    native.require(replay.request.get(origin + "/sessions/whoami").status == 401,
                   "Recovery replay issued a new session")
    replay.close()
    login(recovery, new_email, recovered)
    native.require(profile(recovery) == original, "Recovered credentials did not resolve to the original profile")
    unknown = browser.new_page()
    flow(unknown, "recovery")
    submit(unknown, method, {"email": f"unknown-{secrets.token_hex(12)}@example.test"})
    native.require(unknown.locator('input[name="code"]').count() == 1
                   and not unknown.request.get(origin + "/sessions/whoami").ok,
                   "Unknown account recovery diverged from the code prompt")
    print("PASS: browser recovery, replay rejection, unknown account flow, prior-session revocation and logout", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api", required=True, type=native.loopback_address)
    parser.add_argument("--kratos", required=True, type=native.loopback_address)
    parser.add_argument("--mailpit", required=True, type=native.loopback_address)
    args = parser.parse_args()
    native.API, native.KRATOS, native.MAILPIT = args.api, args.kratos, args.mailpit
    with sync_playwright() as p:
        browser = p.chromium.launch(executable_path=os.environ.get("CF_BROWSER_EXECUTABLE"))
        walk(browser)


if __name__ == "__main__":
    try:
        main()
    except native.SmokeFailure as exc:
        sys.exit(f"FAIL: {exc}")
    except Exception as exc:
        # Playwright diagnostics can contain form values and action tokens.
        tb = exc.__traceback__
        lines = []
        while tb:
            if Path(tb.tb_frame.f_code.co_filename).resolve() == Path(__file__).resolve():
                lines.append(str(tb.tb_lineno))
            tb = tb.tb_next
        sys.exit(f"FAIL: browser lifecycle ({type(exc).__name__}, test lines {', '.join(lines)})")
