#!/usr/bin/env python3
"""Demonstrate consent-gated activation end to end with real Kratos, PostgreSQL and a synthetic webhook."""
import argparse
import hashlib
import hmac
import importlib
import json
import os
import sys
import time
import uuid

m1 = importlib.import_module("m1-smoke")
linking = importlib.import_module("m1-linking-smoke")

CONSENT = "/v1/customer/consent"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("api", "kratos", "mailpit", "sink"):
        parser.add_argument(f"--{name}", required=True, type=m1.loopback_address)
    args = parser.parse_args()
    m1.API, m1.KRATOS, m1.MAILPIT = args.api, args.kratos, args.mailpit
    m1.wait_ready()
    authority = linking.SyntheticSourceAuthority(json.loads(m1.env_value(".env.linking", "CF_DEV_LINKING_ISSUERS"))["importer"])
    source = {"Authorization": "Bearer " + json.loads(m1.env_value(".env", "CF_DEV_TOKENS"))["importer"]}
    operator = {"Authorization": "Bearer " + os.environ["CF_OPERATOR_SESSION"]}
    run = uuid.uuid4().hex[:12]

    # Fresh names per run: definitions, segments and destinations are immutable and the database is reused.
    tier, score = f"act_tier_{run}", f"act_score_{run}"
    for path, definition, readers in ((tier, {"type": "enum", "values": ["gold", "silver"]}, ["operator", "segmentation", "activation"]),
                                      (score, {"type": "integer"}, ["segmentation"])):
        m1.request(m1.API, "POST", f"/v1/operator/attributes/{path}", 201, definition, operator)
        m1.request(m1.API, "POST", f"/v1/operator/attributes/{path}/access", 201, {"readers": readers, "sources": ["importer"]}, operator)
    print(f"1. {tier} may be sent to destinations; {score} may only select an audience")

    epoch = m1.request(m1.API, "GET", "/v1/operator/sources/importer/epoch", 200, headers=operator, read_json=True)["epoch"]
    customers = {}
    for name in ("ada", "bob"):
        record = f"{name}-{run}"
        m1.request(m1.API, "POST", m1.SOURCE, 201, {"set": {tier: "silver"}, "identifiers": [{"namespace": "crm", "value": record}]},
                   {**source, "Idempotency-Key": str(uuid.uuid4())})
        token = m1.create_verified_session(f"activation-{name}-{run}@example.test")
        session = {"X-Session-Token": token}
        anchor = m1.request(m1.API, "PUT", m1.CUSTOMER, 200, headers=session, read_json=True)["id"]
        challenge = authority.enroll("crm", record)
        grant = authority.verify_and_issue("crm", record, challenge, session)
        m1.request(m1.API, "POST", linking.ASSOCIATIONS, 201, {"grant": grant["grant"]}, session)
        # A source event upgrades the linked record.
        event = {"event_id": f"{record}-1", "type": "record.updated.v1", "record": {"namespace": "crm", "value": record},
                 "source_epoch": epoch, "sequence": 1, "occurred_at": "2026-11-01T00:00:00Z", "set": {tier: "gold", score: 900}}
        m1.request(m1.API, "POST", "/v1/source-events", 201, event, {**source, "Idempotency-Key": str(uuid.uuid4())})
        customers[name] = {"token": token, "session": session, "anchor": anchor}
    print("2. Two verified Kratos customers linked to importer records; a source event made both gold with a high score")

    view = m1.request(m1.API, "GET", f"/v1/operator/customers/{customers['ada']['anchor']}/unified-view", 200, headers=operator, read_json=True)
    m1.require([(a["path"], a["values"][0]["value"]) for a in view["attributes"]] == [(tier, "gold")], "The governed view must show the event's tier")
    print(f"3. Governed view of Ada: {tier}=gold, {score} withheld from operators")

    purpose = m1.request(m1.API, "POST", "/v1/operator/consent/purposes", 201, {"name": f"act_email_{run}"}, operator, read_json=True)["id"]
    policy = m1.request(m1.API, "POST", f"/v1/operator/consent/policies/{purpose}", 201, {
        "explanation": "Synthetic demo explanation.", "lawful_basis": "consent", "notice_reference": "demo", "jurisdiction": "DE",
        "locale": "de-DE", "channel": "email", "retention_reference": "demo"}, operator, read_json=True)["id"]
    m1.request(m1.API, "POST", f"/v1/operator/consent/policy-versions/{policy}/state", 200, {"state": "active"}, operator)

    def decide(name, decision):
        session = customers[name]["session"]
        cursor, item = "", None
        while item is None:
            page = m1.request(m1.API, "GET", f"{CONSENT}?limit=100{cursor and '&cursor=' + cursor}", 200, headers=session, read_json=True)
            item = next((i for i in page["items"] if i["purpose_id"] == purpose), None)
            cursor = page.get("next_cursor")
            m1.require(item is not None or cursor, "The demo purpose must be offered to the customer")
        m1.request(m1.API, "POST", f"{CONSENT}/decisions", 201, {"policy_version_id": policy, "decision": decision},
                   {**session, "Idempotency-Key": str(uuid.uuid4()), "If-Match": f'"{item["revision"]}"'})

    decide("ada", "grant")
    decide("bob", "grant")
    print("4. Both customers granted the purpose themselves")

    segment = f"act_gold_{run}"
    m1.request(m1.API, "POST", f"/v1/operator/segments/{segment}", 201, {"purpose_id": purpose, "match": "all", "conditions": [
        {"attribute": tier, "operator": "eq", "value": "gold"}, {"attribute": score, "operator": "gte", "value": 500}]}, operator)
    preview = m1.request(m1.API, "GET", f"/v1/operator/segments/{segment}/preview?limit=100", 200, headers=operator, read_json=True)
    members = {m["anchor_profile_id"] for m in preview["sample"]}
    m1.require({c["anchor"] for c in customers.values()} <= members, "Both customers must be members")
    print(f"5. Segment {segment} previews both customers")

    destination = f"demo_{run}"
    created = m1.request(m1.API, "POST", f"/v1/operator/destinations/{destination}", 201, {"url": f"http://webhook-sink:8090/hook/{run}"},
                         operator, read_json=True)
    secret = bytes.fromhex(created["signing_secret"])
    def run_activation():
        activation = m1.request(m1.API, "POST", f"/v1/operator/segments/{segment}/activations", 201, {"destination": destination},
                                operator, read_json=True)
        m1.require(activation["queued"] == 2, "Both members must be queued")
        decide("bob", "withdraw")
        for _ in range(30):
            status = m1.request(m1.API, "GET", f"/v1/operator/activations/{activation['id']}", 200, headers=operator, read_json=True)
            if all(d["state"] != "pending" for d in status["deliveries"]):
                return {d["anchor_profile_id"]: d for d in status["deliveries"]}
            time.sleep(1)
        raise m1.SmokeFailure("Deliveries did not settle within 30 seconds")

    # The dispatcher runs every two seconds, so it can occasionally send before the withdrawal lands; that
    # send was permitted when it happened. Bob grants again and the demo repeats.
    for _ in range(3):
        by_anchor = run_activation()
        bob = by_anchor[customers["bob"]["anchor"]]
        if bob["state"] != "delivered":
            break
        print("The dispatcher sent before the withdrawal landed; repeating with a new activation")
        decide("bob", "grant")
    print("6. Activation queued both members; Bob withdrew before dispatch")
    ada = by_anchor[customers["ada"]["anchor"]]
    print(json.dumps({"ada": ada, "bob": bob}, indent=2))
    m1.require(ada["state"] == "delivered" and ada["receipts"][-1]["status_code"] == 200, "Ada's delivery must be accepted")
    m1.require(bob["state"] == "suppressed" and bob["reason"] == "consent_not_permitted", "Bob's withdrawal must suppress his delivery")

    received = [r for r in m1.request(args.sink, "GET", "/received", 200, read_json=True) if r["idempotency_key"] == ada["id"]]
    m1.require(len(received) == 1, "Ada's delivery must reach the webhook exactly once")
    m1.require(not any(r["idempotency_key"] == bob["id"] for r in m1.request(args.sink, "GET", "/received", 200, read_json=True)),
               "Bob's suppressed delivery must never reach the webhook")
    body = received[0]["body"]
    # CF-Signature is "t=<unix seconds>" and one "v1=<hex>" per live secret; any v1 may match.
    fields = received[0]["signature"].split(",")
    timestamp = fields[0].removeprefix("t=")
    expected = hmac.new(secret, f"{timestamp}.{body}".encode(), hashlib.sha256).hexdigest()
    m1.require(any(hmac.compare_digest(f.removeprefix("v1="), expected) for f in fields[1:] if f.startswith("v1=")),
               "The delivery signature must verify")
    payload = json.loads(body)
    m1.require(payload["anchor_profile_id"] == customers["ada"]["anchor"] and score not in body, "Only Ada, and only activation-granted attributes")
    print(f"7. The webhook received one signed delivery for Ada: {json.dumps(payload['attributes'])}")

    replay = m1.request(m1.API, "POST", f"/v1/operator/segments/{segment}/activations", 201, {"destination": destination}, operator, read_json=True)
    m1.require(replay["queued"] == 1, "A replay after the withdrawal must queue only Ada")
    print("8. Replaying the activation after the withdrawal queues only Ada")

    # The same activation as a durable job: the API's worker evaluates it outside the request, from one
    # database state, and the job names the activation it published.
    job = m1.request(m1.API, "POST", f"/v1/operator/segments/{segment}/activation-jobs", 202, {"destination": destination},
                     {**operator, "Idempotency-Key": "demo-" + uuid.uuid4().hex}, read_json=True)
    for _ in range(60):
        job = m1.request(m1.API, "GET", f"/v1/operator/audience-jobs/{job['id']}", 200, headers=operator, read_json=True)
        if job["state"] not in ("queued", "running"):
            break
        time.sleep(1)
    m1.require(job["state"] == "completed" and job["members"] == 1 and job.get("activation_id"), "The activation job must complete with only Ada")
    print(f"9. A durable activation job evaluated {job['evaluated']} customers in the background and queued {job['members']} delivery")

    for c in customers.values():
        m1.request(m1.KRATOS, "DELETE", "/self-service/logout/api", 204, {"session_token": c["token"]})
    print("\nPASS: Kratos customer -> verified association -> consent -> source event -> governed view -> segment -> test webhook,"
          " with a withdrawal suppressing delivery")


if __name__ == "__main__":
    try:
        main()
    except m1.SmokeFailure as exc:
        sys.exit(f"FAIL: {exc}")
    except (KeyError, TypeError, ValueError, OSError, linking.ProofRejected) as exc:
        sys.exit(f"FAIL: could not complete the activation demo ({type(exc).__name__})")
