#!/usr/bin/env python3
"""Demonstrate the operator customer view with real Kratos, PostgreSQL and synthetic source proof."""
import argparse
import importlib
import json
import sys
import uuid

m1 = importlib.import_module("m1-smoke")
linking = importlib.import_module("m1-linking-smoke")


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
    operator = {"Authorization": "Bearer " + m1.env_value(".env.m1", "CF_DEV_OPERATOR_TOKEN")}
    run = uuid.uuid4().hex

    def record(name, attributes, extra=()):
        value = f"{name}-{run}"
        identifiers = [{"namespace": "crm", "value": value}, *extra]
        m1.request(m1.API, "POST", m1.SOURCE, 201, {"set": attributes, "identifiers": identifiers},
                   {**source, "Idempotency-Key": str(uuid.uuid4())})
        return value

    gold = record("gold", {"display_name": "Synthetic Customer", "tier": "gold", "internal_score": 42})
    silver = record("silver", {"tier": "silver"})
    shared = record("shared", {"tier": "platinum"}, [{"namespace": "erp", "value": f"alias-{run}"}])

    token = m1.create_verified_session(f"view-{run}@example.test")
    customer = {"X-Session-Token": token}
    other_token = m1.create_verified_session(f"view-other-{run}@example.test")
    other = {"X-Session-Token": other_token}
    anchor = m1.request(m1.API, "PUT", m1.CUSTOMER, 200, headers=customer, read_json=True)
    other_anchor = m1.request(m1.API, "PUT", m1.CUSTOMER, 200, headers=other, read_json=True)
    path = f"/v1/operator/customers/{uuid.UUID(anchor['id'])}/unified-view"

    def read(label):
        view = m1.request(m1.API, "GET", path, 200, headers=operator, read_json=True)
        print(f"\n{label}\n{json.dumps(view, indent=2)}")
        return view

    def by_path(view):
        return {item["path"]: item for item in view["attributes"]}

    empty = read("Before any proof: similar source data links nothing")
    m1.require(empty["records"] == [] and empty["attributes"] == [], "An unlinked customer must see nothing")

    def link(value):
        challenge = authority.enroll("crm", value)
        grant = authority.verify_and_issue("crm", value, challenge, customer)
        association = m1.request(m1.API, "POST", linking.ASSOCIATIONS, 201, {"grant": grant["grant"]}, customer, read_json=True)
        return grant["id"], f"{linking.ASSOCIATIONS}/{uuid.UUID(association['id'])}"

    gold_grant, _ = link(gold)
    _, silver_link = link(silver)
    link(shared)

    view = read("Three verified records: values are listed with provenance and never merged")
    attributes = by_path(view)
    m1.require(set(attributes) == {"display_name", "tier"}, "The view disclosed an attribute outside the policy")
    m1.require(not attributes["display_name"]["conflict"], "One value cannot conflict")
    m1.require(attributes["tier"]["conflict"] and {v["value"] for v in attributes["tier"]["values"]} == {"gold", "silver"},
               "Disagreeing records must both be visible and reported as a conflict")
    m1.require(all(v["mutation_id"] and v["written_at"] and v["association_id"] for v in attributes["tier"]["values"]),
               "Every value needs provenance")
    serialized = json.dumps(view)
    m1.require("internal_score" not in serialized and all(v["value"] != 42 for item in attributes.values() for v in item["values"]),
               "A withheld attribute leaked")
    m1.require("platinum" not in serialized, "An ambiguous record disclosed a value")
    m1.require(sorted(r["status"] for r in view["records"]) == ["included", "included", "multiple_identifiers"],
               "The record with two identifiers must be reported as ambiguous")
    m1.require(sum(r["withheld_attributes"] for r in view["records"]) == 1, "The withheld attribute must be counted")

    m1.require(m1.request(m1.API, "GET", m1.CUSTOMER, 200, headers=customer, read_json=True) == anchor,
               "The view changed the customer anchor")
    print("\nThe customer anchor is unchanged:", json.dumps(anchor))

    m1.request(m1.API, "GET", path, 401, headers=customer)
    m1.request(m1.API, "GET", path, 401, headers=source)
    m1.request(m1.API, "GET", path, 401, headers=authority.issuer_headers)
    print("Customer session, source credential and linking issuer credential: 401")
    isolated = m1.request(m1.API, "GET", f"/v1/operator/customers/{uuid.UUID(other_anchor['id'])}/unified-view", 200,
                          headers=operator, read_json=True)
    m1.require(isolated["records"] == [], "Another customer's view must not contain these records")
    print("Another customer's view is empty")

    m1.request(m1.API, "DELETE", silver_link, 204, headers=customer)
    tier = by_path(read("After the customer unlinks one record: its value is gone"))["tier"]
    m1.require(not tier["conflict"] and [v["value"] for v in tier["values"]] == ["gold"], "A revoked record stayed visible")

    m1.request(m1.API, "DELETE", f"{linking.GRANTS}/{uuid.UUID(gold_grant)}", 204, headers=authority.issuer_headers)
    final = read("After the issuer revokes the other link: only the ambiguous record remains, with no values")
    m1.require(final["attributes"] == [] and len(final["records"]) == 1, "An issuer revocation stayed visible")

    for session in (token, other_token):
        m1.request(m1.KRATOS, "DELETE", "/self-service/logout/api", 204, {"session_token": session})
    print("\nPASS: operator customer view with provenance, conflicts, withheld attributes and revocation")


if __name__ == "__main__":
    try:
        main()
    except m1.SmokeFailure as exc:
        sys.exit(f"FAIL: {exc}")
    except (KeyError, TypeError, ValueError, OSError, linking.ProofRejected) as exc:
        sys.exit(f"FAIL: could not complete the customer view demo ({type(exc).__name__})")
