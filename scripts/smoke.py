#!/usr/bin/env python3
"""Exercise the running local API with synthetic data; never print credentials."""
import json
import shlex
import sys
import urllib.error
import urllib.request
from pathlib import Path

env = {}
for line in (Path(__file__).resolve().parents[1] / '.env').read_text().splitlines():
    if line and not line.startswith('#'):
        key, value = line.split('=', 1)
        env[key] = shlex.split(value)[0]
token = json.loads(env['CF_DEV_TOKENS'])['console']
port = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
if not 1 <= port <= 65535:
    raise SystemExit('Expected a published API port')
base = f'http://127.0.0.1:{port}'

def request(method, path, payload=None, key=None, etag=None, authenticated=True):
    headers = {'Content-Type': 'application/json'}
    if authenticated:
        headers['Authorization'] = 'Bearer ' + token
    if key:
        headers['Idempotency-Key'] = key
    if etag:
        headers['If-Match'] = etag
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(base + path, data=data, method=method, headers=headers)
    try:
        response = urllib.request.urlopen(req, timeout=10)
    except urllib.error.HTTPError as e:
        response = e
    with response:
        return response.status, json.load(response)

assert request('GET', '/v1/profiles', authenticated=False)[0] == 401
payload = {'set': {'display_name': 'Synthetic Ada', 'active': True},
           'identifiers': [{'namespace': 'crm', 'value': 'synthetic-smoke-001'}]}
status, receipt = request('POST', '/v1/profiles', payload, 'synthetic-smoke-create-0001')
assert status == 201, status
assert request('POST', '/v1/profiles', payload, 'synthetic-smoke-create-0001')[1] == receipt
path = '/v1/profiles/' + receipt['id']
status, updated = request('PATCH', path, {'set': {'display_name': 'Synthetic Grace'}},
                    'synthetic-smoke-update-0001', '"1"')
assert status == 200, status
status, profile = request('GET', path)
assert status == 200 and profile['id'] == receipt['id'] and profile['records'][0]['version'] == 2
assert profile['attributes']['display_name'] == 'Synthetic Grace'
# Earlier runs may have added decisions to this shared synthetic profile.
# Assert this smoke flow's exact decisions by receipt, rather than total audit length.
expected = {(receipt['mutation_id'], 'display_name'), (receipt['mutation_id'], 'active'),
            (updated['mutation_id'], 'display_name')}
owned = [p for p in profile['provenance'] if p['mutation_id'] in {receipt['mutation_id'], updated['mutation_id']}]
assert len(owned) == 3 and {(p['mutation_id'], p['path']) for p in owned} == expected
assert all(p['source'] == 'console' and p['outcome'] == 'accepted' and p['rule'] == 'source-record' for p in owned)
assert profile['identifiers'] == [{'profile_id': receipt['id'], 'source': 'console', 'namespace': 'crm', 'value': 'synthetic-smoke-001'}]
assert request('GET', '/v1/profiles?limit=20')[0] == 200
print('PASS: live authentication, create, replay, update, retrieval, identifiers, provenance, listing')
print('Synthetic profile:', receipt['id'])
