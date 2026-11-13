#!/usr/bin/env python3
"""Demonstrate a consent-gated segment preview with real Kratos customers, PostgreSQL and synthetic sources."""
import argparse
import importlib
import json
import os
import sys
import uuid

m1 = importlib.import_module("m1-smoke")
linking = importlib.import_module("m1-linking-smoke")

CONSENT = "/v1/customer/consent"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api", required=True, type=m1.loopback_address)
    parser.add_argument("--kratos", required=True, type=m1.loopback_address)
    parser.add_argument("--mailpit", required=True, type=m1.loopback_address)
    args = parser.parse_args()
    m1.API, m1.KRATOS, m1.MAILPIT = args.api, args.kratos, args.mailpit
    m1.wait_ready()
    authority = linking.SyntheticSourceAuthority(json.loads(m1.env_value(".env.linking", "CF_DEV_LINKING_ISSUERS"))["importer"])
    source = {"Authorization": "Bearer " + json.loads(m1.env_value(".env", "CF_DEV_TOKENS"))["importer"]}
    operator = {"Authorization": "Bearer " + os.environ["CF_OPERATOR_SESSION"]}
    run = uuid.uuid4().hex[:12]

    # Fresh names per run: definitions and segments are immutable and the stack's database is reused.
    tier, score = f"demo_tier_{run}", f"demo_score_{run}"
    for path, definition, readers in ((tier, {"type": "enum", "values": ["gold", "silver"]}, ["operator", "segmentation"]),
                                      (score, {"type": "integer"}, ["segmentation"])):
        m1.request(m1.API, "POST", f"/v1/operator/attributes/{path}", 201, definition, operator)
        m1.request(m1.API, "POST", f"/v1/operator/attributes/{path}/access", 201, {"readers": readers, "sources": ["importer"]}, operator)
    print(f"1. Governed attributes: {tier} (operator, segmentation) and {score} (segmentation only)")

    def record(name, attributes):
        value = f"{name}-{run}"
        m1.request(m1.API, "POST", m1.SOURCE, 201, {"set": attributes, "identifiers": [{"namespace": "crm", "value": value}]},
                   {**source, "Idempotency-Key": str(uuid.uuid4())})
        return value

    customers = {}
    for name, attributes in (("ada", {tier: "gold", score: 120}), ("bob", {tier: "gold", score: 40}), ("cy", {tier: "silver", score: 300})):
        value = record(name, attributes)
        token = m1.create_verified_session(f"segment-{name}-{run}@example.test")
        session = {"X-Session-Token": token}
        anchor = m1.request(m1.API, "PUT", m1.CUSTOMER, 200, headers=session, read_json=True)["id"]
        challenge = authority.enroll("crm", value)
        grant = authority.verify_and_issue("crm", value, challenge, session)
        m1.request(m1.API, "POST", linking.ASSOCIATIONS, 201, {"grant": grant["grant"]}, session)
        customers[name] = {"token": token, "session": session, "anchor": anchor}
    print("2. Ingested three synthetic source records and linked each to a verified Kratos customer")

    view = m1.request(m1.API, "GET", f"/v1/operator/customers/{customers['ada']['anchor']}/unified-view", 200, headers=operator, read_json=True)
    m1.require([a["path"] for a in view["attributes"]] == [tier], "The operator view must withhold the segmentation-only score")
    print(f"3. Operator view of Ada discloses {tier}={view['attributes'][0]['values'][0]['value']} and withholds {score}")

    purpose = m1.request(m1.API, "POST", "/v1/operator/consent/purposes", 201, {"name": f"demo_email_{run}"}, operator, read_json=True)["id"]
    policy = m1.request(m1.API, "POST", f"/v1/operator/consent/policies/{purpose}", 201, {
        "explanation": "Synthetic demo explanation.", "lawful_basis": "consent", "notice_reference": "demo", "jurisdiction": "DE",
        "locale": "de-DE", "channel": "email", "retention_reference": "demo"}, operator, read_json=True)["id"]
    m1.request(m1.API, "POST", f"/v1/operator/consent/policy-versions/{policy}/state", 200, {"state": "active"}, operator)

    def decide(name, decision):
        session = customers[name]["session"]
        cursor = ""
        while True:
            page = m1.request(m1.API, "GET", f"{CONSENT}?limit=100{cursor and '&cursor=' + cursor}", 200, headers=session, read_json=True)
            item = next((i for i in page["items"] if i["purpose_id"] == purpose), None)
            if item or not page.get("next_cursor"):
                break
            cursor = page["next_cursor"]
        m1.require(item is not None, "The demo purpose must be offered to the customer")
        m1.request(m1.API, "POST", f"{CONSENT}/decisions", 201, {"policy_version_id": policy, "decision": decision},
                   {**session, "Idempotency-Key": str(uuid.uuid4()), "If-Match": f'"{item["revision"]}"'})

    for name in ("ada", "bob", "cy"):
        decide(name, "grant")
    print("4. All three customers granted the demo purpose themselves")

    segment = f"/v1/operator/segments/demo_high_value_{run}"
    m1.request(m1.API, "POST", segment, 201, {"purpose_id": purpose, "match": "all", "conditions": [
        {"attribute": tier, "operator": "eq", "value": "gold"},
        {"attribute": score, "operator": "gte", "value": 100}]}, operator)
    print(f"5. Segment: {tier} = gold AND {score} >= 100, members must permit the demo purpose")

    def members(label):
        preview = m1.request(m1.API, "GET", f"{segment}/preview?limit=100", 200, headers=operator, read_json=True)
        names = sorted(n for n, c in customers.items() if any(m["anchor_profile_id"] == c["anchor"] for m in preview["sample"]))
        print(f"\n{label}: members {names}; population {preview['population']}, excluded {preview['excluded']}")
        return preview, names

    preview, names = members("6. Preview")
    m1.require(names == ["ada"], "Only Ada is gold with a high score")
    ada = next(m for m in preview["sample"] if m["anchor_profile_id"] == customers["ada"]["anchor"])
    print(json.dumps(ada, indent=2))
    m1.require(all(c["matched"] and c["evidence"] and c["evidence"][0]["mutation_id"] for c in ada["conditions"]),
               "Each condition must cite the provenance that decided it")
    m1.require('"value"' not in json.dumps(preview) and "gold" not in json.dumps(preview), "A preview must never disclose values")

    decide("ada", "withdraw")
    _, names = members("7. After Ada withdraws consent")
    m1.require(names == [], "A withdrawal must remove the member on the next preview")

    m1.request(m1.API, "GET", f"{segment}/preview", 401, headers=customers["ada"]["session"])
    m1.request(m1.API, "GET", f"{segment}/preview", 401, headers=source)
    print("Customer session and source credential: 401")
    for c in customers.values():
        m1.request(m1.KRATOS, "DELETE", "/self-service/logout/api", 204, {"session_token": c["token"]})
    print("\nPASS: sources -> governed view -> consent -> segment -> preview, and a withdrawal changes membership")


if __name__ == "__main__":
    try:
        main()
    except m1.SmokeFailure as exc:
        sys.exit(f"FAIL: {exc}")
    except (KeyError, TypeError, ValueError, OSError, linking.ProofRejected) as exc:
        sys.exit(f"FAIL: could not complete the segment demo ({type(exc).__name__})")
