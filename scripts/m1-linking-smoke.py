#!/usr/bin/env python3
"""Exercise verified associations with real Kratos, PostgreSQL and synthetic source proof."""
import argparse
import hmac
import importlib
import json
import secrets
import sys
import time
import uuid

m1 = importlib.import_module("m1-smoke")
GRANTS = "/v1/linking/grants"
ASSOCIATIONS = "/v1/customer/associations"


class ProofRejected(Exception):
    pass


class SyntheticSourceAuthority:
    """Test-only authority. Simulates delivery of a secret to a separate source account."""

    def __init__(self, issuer_token):
        self.issuer_headers = {"Authorization": "Bearer " + issuer_token}
        self.challenges = {}

    def enroll(self, namespace, record):
        challenge = secrets.token_urlsafe(32)
        self.challenges[(namespace, record)] = challenge, time.monotonic() + 300
        # The test plays the holder of this source account and receives this
        # challenge out of band. A contact ID or a verified email cannot obtain it.
        return challenge

    def verify_and_issue(self, namespace, record, response, customer_headers):
        key = namespace, record
        challenge = self.challenges.get(key)
        if challenge is None or time.monotonic() >= challenge[1] or not hmac.compare_digest(challenge[0], response):
            raise ProofRejected
        whoami = m1.request(m1.KRATOS, "GET", "/sessions/whoami", 200,
                            headers=customer_headers, read_json=True)
        identity = whoami["identity"]
        email = identity["traits"]["email"].strip().lower()
        if not (whoami["active"] and identity["state"] == "active" and any(
                item["verified"] and item["via"] == "email" and item["value"] == email
                for item in identity["verifiable_addresses"])):
            raise ProofRejected
        del self.challenges[key]
        claim = {
            "namespace": namespace,
            "record": record,
            "customer_subject": str(uuid.UUID(identity["id"])),
            "proof_method": "synthetic_challenge",
            "proof_reference": str(uuid.uuid4()),
        }
        return m1.request(m1.API, "POST", GRANTS, 201, claim, self.issuer_headers, read_json=True)


def fixture_record(source, index):
    path = m1.ROOT / "tests" / "fixtures" / "enterprise" / "sample" / "sources" / source / "records.ndjson"
    return json.loads(path.read_text().splitlines()[index])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api", required=True, type=m1.loopback_address)
    parser.add_argument("--kratos", required=True, type=m1.loopback_address)
    parser.add_argument("--mailpit", required=True, type=m1.loopback_address)
    args = parser.parse_args()
    m1.API, m1.KRATOS, m1.MAILPIT = args.api, args.kratos, args.mailpit
    m1.wait_ready()
    source_tokens = json.loads(m1.env_value(".env", "CF_DEV_TOKENS"))
    authority = SyntheticSourceAuthority(json.loads(m1.env_value(".env.linking", "CF_DEV_LINKING_ISSUERS"))["importer"])
    source_auth = {"Authorization": "Bearer " + source_tokens["importer"]}
    other_source_auth = {"Authorization": "Bearer " + source_tokens["console"]}
    suffix = uuid.uuid4().hex
    email = f"linking-{suffix}@example.test"
    first_crm = fixture_record("salesforce", 0)
    duplicate_crm = fixture_record("salesforce", 1)
    historical_erp = fixture_record("erp", 0)
    m1.require(first_crm["Email"] == duplicate_crm["Email"] and first_crm["Id"] != duplicate_crm["Id"],
               "The synthetic CRM fixture must represent duplicate identifiers for a matching email")

    def import_record(namespace, identifier, attributes):
        record = f"{identifier}-{suffix}"
        receipt = m1.request(m1.API, "POST", m1.SOURCE, 201, {
            "set": attributes,
            "identifiers": [{"namespace": namespace, "value": record}],
        }, {**source_auth, "Idempotency-Key": str(uuid.uuid4())}, read_json=True)
        return record, receipt["id"]

    crm, crm_id = import_record("salesforce", first_crm["Id"],
                                {"email": email, "created": first_crm["CreatedDate"]})
    duplicate, _ = import_record("salesforce", duplicate_crm["Id"],
                                 {"email": email, "created": duplicate_crm["CreatedDate"]})
    erp, _ = import_record("erp", historical_erp["KUNDENNR"],
                           {"created": historical_erp["ANGELEGT"], "active": historical_erp["AKTIV"]})

    token = m1.create_verified_session(email)
    customer = {"X-Session-Token": token}
    other_token = m1.create_verified_session(f"linking-other-{suffix}@example.test")
    other_customer = {"X-Session-Token": other_token}
    anchor = m1.request(m1.API, "PUT", m1.CUSTOMER, 200, headers=customer, read_json=True)
    m1.require(anchor["attributes"] == {} and anchor["id"] != crm_id, "Customer anchor must be separate")
    m1.require(m1.request(m1.API, "GET", ASSOCIATIONS, 200, headers=customer, read_json=True) == {"items": []},
               "Similar source data must not automatically link")
    m1.request(m1.API, "POST", GRANTS, 401, {}, source_auth)
    m1.request(m1.API, "POST", GRANTS, 401, {}, customer)
    m1.request(m1.API, "GET", ASSOCIATIONS, 401, headers=authority.issuer_headers)
    m1.request(m1.API, "GET", m1.SOURCE, 401, headers=authority.issuer_headers)
    m1.request(m1.API, "GET", m1.OPERATOR, 401, headers=authority.issuer_headers)

    def refuse(namespace, record, response):
        try:
            authority.verify_and_issue(namespace, record, response, customer)
        except ProofRejected:
            return
        raise m1.SmokeFailure("Synthetic authority accepted invalid, wrong-target or replayed proof")

    def link(namespace, record):
        challenge = authority.enroll(namespace, record)
        refuse(namespace, record, record)
        grant = authority.verify_and_issue(namespace, record, challenge, customer)
        refuse(namespace, record, challenge)
        m1.request(m1.API, "POST", ASSOCIATIONS, 409, {"grant": grant["grant"]}, other_customer)
        association = m1.request(m1.API, "POST", ASSOCIATIONS, 201, {"grant": grant["grant"]},
                                 customer, read_json=True)
        m1.require(set(association) == {"id", "source", "linked_at"}, "Association leaked source details")
        m1.request(m1.API, "POST", ASSOCIATIONS, 409, {"grant": grant["grant"]}, customer)
        return grant["id"], ASSOCIATIONS + "/" + str(uuid.UUID(association["id"]))

    refuse("salesforce", duplicate, duplicate)
    crm_grant, crm_link = link("salesforce", crm)
    _, erp_link = link("erp", erp)
    m1.request(m1.API, "GET", crm_link, 404, headers=other_customer)
    m1.request(m1.API, "DELETE", crm_link, 404, headers=other_customer)
    listing = m1.request(m1.API, "GET", ASSOCIATIONS + "?limit=1", 200, headers=customer, read_json=True)
    more = m1.request(m1.API, "GET", ASSOCIATIONS + "?limit=1&cursor=" + str(uuid.UUID(listing["next_cursor"])),
                      200, headers=customer, read_json=True)
    m1.require(len(listing["items"]) == len(more["items"]) == 1 and
               {listing["items"][0]["id"], more["items"][0]["id"]} ==
               {crm_link.rsplit("/", 1)[1], erp_link.rsplit("/", 1)[1]}, "Pagination lost or exposed an association")

    m1.require(m1.request(m1.API, "GET", m1.CUSTOMER, 200, headers=customer, read_json=True) == anchor,
               "Association modified the original profile anchor")
    m1.request(m1.API, "GET", m1.CUSTOMER + "/" + crm_id, 404, headers=customer)
    mutation = {"set": {"active": True}}
    patch = {"Idempotency-Key": str(uuid.uuid4()), "If-Match": '"1"'}
    m1.request(m1.API, "PATCH", m1.SOURCE + "/" + crm_id, 403, mutation, {**other_source_auth, **patch})
    m1.request(m1.API, "PATCH", m1.SOURCE + "/" + crm_id, 200, mutation, {**source_auth, **patch})
    m1.request(m1.API, "DELETE", GRANTS + "/" + str(uuid.UUID(crm_grant)), 204, headers=authority.issuer_headers)
    m1.request(m1.API, "GET", crm_link, 404, headers=customer)
    m1.request(m1.API, "DELETE", erp_link, 204, headers=customer)
    m1.request(m1.API, "GET", erp_link, 404, headers=customer)
    for session in (token, other_token):
        m1.request(m1.KRATOS, "DELETE", "/self-service/logout/api", 204, {"session_token": session})
    m1.request(m1.API, "GET", ASSOCIATIONS, 401, headers=customer)
    print("PASS: independent synthetic proof, CRM and ERP links, replay denial, ownership and revocation")


if __name__ == "__main__":
    try:
        main()
    except m1.SmokeFailure as exc:
        sys.exit(f"FAIL: {exc}")
    except (KeyError, TypeError, ValueError, OSError, ProofRejected) as exc:
        sys.exit(f"FAIL: could not complete linking smoke ({type(exc).__name__})")
