#!/usr/bin/env python3
"""Demonstrate a resumable source import against the running M1 stack. Synthetic data only."""
import argparse
import importlib
import json
import sys
import urllib.error
import urllib.request
import uuid

m1 = importlib.import_module("m1-smoke")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api", required=True, type=m1.loopback_address)
    args = parser.parse_args()
    m1.API = args.api
    source = {"Authorization": "Bearer " + json.loads(m1.env_value(".env", "CF_DEV_TOKENS"))["importer"]}
    run = uuid.uuid4().hex[:12]

    def rows(start, end):
        out = []
        for i in range(start, end + 1):
            record = {"namespace": "crm", "value": f"import-{run}-{i:05d}"}
            if i == 1500:  # an oversized line stops the batch; the rows before it stay committed
                out.append(json.dumps({"record": record, "set": {"note": "x" * 70_000}}))
            elif i % 400 == 0:  # a malformed row is quarantined, never stored
                out.append('{"record": broken')
            else:
                out.append(json.dumps({"record": record, "set": {"display_name": f"Synthetic Kunde {i} – Zoë", "rank": i}}))
        return out

    imp = m1.request(m1.API, "POST", "/v1/imports", 201, {"max_errors": 10}, {**source, "Idempotency-Key": str(uuid.uuid4())}, read_json=True)

    def batch(offset, lines):
        req = urllib.request.Request(f"{m1.API}/v1/imports/{imp['id']}/rows", data="\n".join(lines).encode(), method="POST",
                                     headers={**source, "Content-Type": "application/x-ndjson", "Import-Offset": str(offset)})
        try:
            with urllib.request.urlopen(req, timeout=60) as res:
                return res.status, json.load(res)
        except urllib.error.HTTPError as err:
            return err.code, json.load(err)

    def show(label, progress):
        print(f"{label}: checkpoint {progress['checkpoint']}, created {progress['created']}, quarantined {progress['quarantined']}, state {progress['state']}")

    status, progress = batch(1, rows(1, 1000))
    m1.require(status == 200, "The first batch must apply")
    show("Batch 1-1000", progress)
    status, problem = batch(1001, rows(1001, 2000))
    m1.require(status == 413, "The oversized line must stop the batch")
    progress = m1.request(m1.API, "GET", f"/v1/imports/{imp['id']}", 200, headers=source, read_json=True)
    show(f"Batch 1001-2000 stopped ({problem['detail']})", progress)
    m1.require(progress["checkpoint"] == 1499, "Rows before the oversized line must be committed")
    status, _ = batch(1001, rows(1001, 2000))
    m1.require(status == 409, "Resending from the old offset must be refused")
    print("Resending from 1001 is refused: the checkpoint says resume at 1500")
    fixed = [json.dumps({"record": {"namespace": "crm", "value": f"import-{run}-01500"}, "set": {"rank": 1500}})] + rows(1501, 2500)
    status, progress = batch(1500, fixed[:1000])
    status, progress = batch(2500, fixed[1000:])
    m1.require(status == 200, "The resumed batches must apply")
    progress = m1.request(m1.API, "POST", f"/v1/imports/{imp['id']}/complete", 200, headers={**source, "Idempotency-Key": str(uuid.uuid4())}, read_json=True)
    show("Resumed and completed", progress)
    m1.require(progress["checkpoint"] == 2500 and progress["created"] + progress["quarantined"] == 2500, "Every line is accounted for once")
    print("Quarantine:", ", ".join(f"line {q['line']} {q['reason']}" for q in progress["quarantine"]))
    print("\nPASS: 2,500 rows imported once each across a failure and resume; malformed rows quarantined without content")


if __name__ == "__main__":
    try:
        main()
    except m1.SmokeFailure as exc:
        sys.exit(f"FAIL: {exc}")
    except (KeyError, TypeError, ValueError, OSError) as exc:
        sys.exit(f"FAIL: could not complete the import demo ({type(exc).__name__})")
