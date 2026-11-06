#!/usr/bin/env python3
"""Exercise the running local API with synthetic data; never print credentials."""
import json
import shlex
import urllib.error
import urllib.request
from pathlib import Path

env = {}
for line in (Path(__file__).resolve().parents[1] / '.env').read_text().splitlines():
    if line and not line.startswith('#'):
        key, value = line.split('=', 1)
        env[key] = shlex.split(value)[0]
token = json.loads(env['CF_DEV_TOKENS'])['console']
base = 'http://127.0.0.1:8080'

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
status, _ = request('PATCH', path, {'set': {'display_name': 'Synthetic Grace'}},
                    'synthetic-smoke-update-0001', '"1"')
assert status == 200, status
status, profile = request('GET', path)
assert status == 200 and profile['version'] == 2
assert profile['attributes']['display_name'] == 'Synthetic Grace'
assert len(profile['provenance']) == 3 and len(profile['identifiers']) == 1
assert request('GET', '/v1/profiles?limit=20')[0] == 200
print('PASS: live authentication, create, replay, update, retrieval, identifiers, provenance, listing')
print('Synthetic profile:', receipt['id'])
