// Live OpenAPI validation of every customer, linking and operator operation on the M1 stack,
// using real Kratos sessions and captured mail. Run it through `make m1-contracts`.
import { SQL } from 'bun';
import assert from 'node:assert/strict';
import { ajv, contract } from './openapi.ts';

function required(name: string): string {
  const value = process.env[name];
  assert.ok(value, `${name} is required; run make m1-contracts`);
  return value;
}
const api = required('CF_CONTRACT_BASE_URL');
const kratos = required('CF_CONTRACT_KRATOS_URL');
const mailpit = required('CF_CONTRACT_MAILPIT_URL');
const browserOrigin = required('CF_CONTRACT_BROWSER_ORIGIN');
const operatorToken = required('CF_DEV_OPERATOR_TOKEN');
const sourceTokens = JSON.parse(required('CF_DEV_TOKENS'));
const issuerToken = JSON.parse(required('CF_DEV_LINKING_ISSUERS')).importer;
const database = new SQL(required('CF_CONTRACT_DATABASE_URL'));

type Headers = Record<string, string>;
type Options = {headers?: Headers; body?: unknown; raw?: string};

const inScope = (path: string) => /^\/v1\/(customer|linking|operator)\//.test(path);
const operations = new Map<string, {route: RegExp; path: string; operations: Record<string, any>}>();
for (const [path, item] of Object.entries<any>(contract.paths)) {
  if (!inScope(path)) continue;
  const route = new RegExp('^' + path.replace(/\{[^}]+\}/g, '[^/]+') + '$');
  operations.set(path, {route, path, operations: item});
  for (const method of ['get', 'put', 'post', 'delete', 'patch']) {
    const responses = item[method]?.responses;
    if (!responses) continue;
    assert.ok(!responses.default, `${method} ${path}: list each status explicitly instead of a default`);
    for (const status of Object.keys(responses)) assert.match(status, /^[1-5][0-9]{2}$/, `${method} ${path}: ${status}`);
  }
}

const exercised = new Map<string, Set<number>>();

function operationFor(method: string, url: string) {
  const path = url.split('?')[0];
  for (const entry of operations.values()) {
    const operation = entry.operations[method.toLowerCase()];
    if (operation && entry.route.test(path)) return operation;
  }
  throw new Error(`${method} ${path}: no documented operation`);
}

// Sends one request and holds the response to the documented status, media type, schema and cache policy.
async function call(method: string, url: string, expected: number, options: Options = {}) {
  const operation = operationFor(method, url);
  const label = `${method} ${operation.operationId} → ${expected}`;
  const headers: Headers = {...options.headers};
  let body: string | undefined = options.raw;
  if (options.body !== undefined) {
    body = JSON.stringify(options.body);
    headers['Content-Type'] ??= 'application/json';
  }
  const response = await fetch(api + url, {method, headers, body, redirect: 'error'});
  assert.equal(response.status, expected, `${label}: got ${response.status}`);
  const documented = operation.responses[String(response.status)];
  assert.ok(documented, `${label}: status is not documented`);
  assert.equal(response.headers.get('cache-control'), 'no-store', `${label}: cache policy`);
  assert.equal(response.headers.get('x-content-type-options'), 'nosniff', `${label}: nosniff`);
  for (const name of Object.keys(documented.headers ?? {})) assert.ok(response.headers.get(name), `${label}: missing ${name}`);
  let parsed: any;
  if (documented.content) {
    const [media] = Object.keys(documented.content);
    assert.equal(response.headers.get('content-type')?.split(';')[0], media, `${label}: media type`);
    parsed = await response.json();
    const validate = ajv.compile(documented.content[media].schema);
    assert.ok(validate(parsed), `${label}: ${ajv.errorsText(validate.errors)}`);
  } else {
    assert.equal(await response.text(), '', `${label}: expected an empty body`);
  }
  const seen = exercised.get(operation.operationId) ?? new Set<number>();
  exercised.set(operation.operationId, seen.add(expected));
  console.log(`PASS: ${label}`);
  return parsed;
}

// Identity-provider and test-data traffic is outside the contract, so it is not validated here.
async function plain(base: string, method: string, url: string, expected: number, options: Options = {}) {
  const headers: Headers = {Accept: 'application/json', ...options.headers};
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  const response = await fetch(base + url, {method, headers, body: options.body === undefined ? undefined : JSON.stringify(options.body), redirect: 'manual'});
  assert.equal(response.status, expected, `${method} ${url.split('?')[0]}: got ${response.status}`);
  return response;
}
const plainJSON = async (base: string, method: string, url: string, expected: number, options: Options = {}) =>
  (await plain(base, method, url, expected, options)).json() as Promise<any>;

async function verificationCode(email: string, flow: string): Promise<string> {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    const inbox = await plainJSON(mailpit, 'GET', '/api/v1/messages?limit=50', 200);
    for (const message of inbox.messages ?? []) {
      if (!message.To.some((to: any) => to.Address === email)) continue;
      const detail = await plainJSON(mailpit, 'GET', `/api/v1/message/${encodeURIComponent(message.ID)}`, 200);
      for (const link of detail.Text.match(/https?:\/\/\S+/g) ?? []) {
        const url = new URL(link);
        if (url.pathname === '/self-service/verification' && url.searchParams.get('flow') === flow && url.searchParams.get('code')) {
          return url.searchParams.get('code')!;
        }
      }
    }
    await Bun.sleep(500);
  }
  throw new Error('Timed out waiting for the synthetic verification mail');
}

const nativeFlow = async (kind: string) => (await plainJSON(kratos, 'GET', `/self-service/${kind}/api`, 200)).id as string;

// Registers and verifies a synthetic account through real Kratos, returning its credentials and native token.
async function verifiedCustomer() {
  const email = `contract-${crypto.randomUUID()}@example.test`;
  const password = crypto.randomUUID() + crypto.randomUUID();
  const registration = await plainJSON(kratos, 'POST', `/self-service/registration?flow=${await nativeFlow('registration')}`, 200,
    {body: {method: 'password', password, traits: {email}}});
  const login = {method: 'password', identifier: email, password};
  const blocked = await plainJSON(kratos, 'POST', `/self-service/login?flow=${await nativeFlow('login')}`, 403, {body: login});
  const flow = blocked.error.details.continue_with.find((step: any) => step.action === 'show_verification_ui').flow.id;
  await plainJSON(kratos, 'POST', `/self-service/verification?flow=${flow}`, 200, {body: {method: 'code', code: await verificationCode(email, flow)}});
  const authenticated = await plainJSON(kratos, 'POST', `/self-service/login?flow=${await nativeFlow('login')}`, 200, {body: login});
  return {email, password, subject: registration.identity.id as string, native: {'X-Session-Token': authenticated.session_token as string}};
}

function cookiePairs(response: Response) {
  return response.headers.getSetCookie().map(cookie => cookie.split(';')[0]);
}

// Logs in through the real Kratos browser flow and returns the session cookie the API must accept.
async function browserCookie(email: string, password: string) {
  const start = await plain(kratos, 'GET', '/self-service/login/browser', 200);
  const flow: any = await start.json();
  const csrf = flow.ui.nodes.find((node: any) => node.attributes.name === 'csrf_token').attributes.value;
  const submitted = await plain(kratos, 'POST', `/self-service/login?flow=${flow.id}`, 200, {
    headers: {Cookie: cookiePairs(start).join('; ')},
    body: {method: 'password', identifier: email, password, csrf_token: csrf},
  });
  const name = `cf_customer_${new URL(browserOrigin).port}`;
  const pair = cookiePairs(submitted).find(cookie => cookie.startsWith(name + '='));
  assert.ok(pair, 'Kratos did not issue the configured browser session cookie');
  return pair;
}

const uuid = () => crypto.randomUUID();
const hex = () => uuid().replaceAll('-', '');
const bearer = (token: string): Headers => ({Authorization: `Bearer ${token}`});
const invalidSession = {'X-Session-Token': 'ory_st_' + hex() + hex()};
const operator = bearer(operatorToken);
const issuer = bearer(issuerToken);
const importer = bearer(sourceTokens.importer);

const customer = await verifiedCustomer();
const other = await verifiedCustomer();
const cookie = await browserCookie(customer.email, customer.password);
const sameOrigin: Headers = {Cookie: cookie, Origin: browserOrigin, 'Sec-Fetch-Site': 'same-origin'};
const profilePath = '/v1/customer/profile';
const associationsPath = '/v1/customer/associations';
const grantsPath = '/v1/linking/grants';

await call('GET', profilePath, 401);
await call('GET', profilePath, 401, {headers: invalidSession});
await call('GET', profilePath, 401, {headers: importer});
await call('GET', profilePath, 401, {headers: {...customer.native, Cookie: cookie}});
await call('GET', profilePath, 401, {headers: {Cookie: `cf_customer_1=${hex()}`}});
await call('GET', profilePath, 404, {headers: customer.native});
await call('GET', `${profilePath}?id=${uuid()}`, 400, {headers: customer.native});
await call('PUT', profilePath, 401);
await call('PUT', profilePath, 403, {headers: {Cookie: cookie, Origin: 'https://attacker.example'}});
await call('PUT', profilePath, 403, {headers: {...sameOrigin, 'Sec-Fetch-Site': 'cross-site'}});
await call('PUT', profilePath, 415, {headers: {...sameOrigin, 'Content-Type': 'text/plain'}, raw: 'synthetic'});
await call('PUT', profilePath, 400, {headers: customer.native, body: {id: uuid()}});
const anchor = await call('PUT', profilePath, 200, {headers: sameOrigin});
assert.deepEqual(await call('GET', profilePath, 200, {headers: customer.native}), anchor);
assert.deepEqual(await call('GET', profilePath, 200, {headers: {Cookie: cookie}}), anchor);
const otherAnchor = await call('PUT', profilePath, 200, {headers: other.native});

// The unified view discloses only governed attributes, so this run defines and grants its own.
const viewAttribute = `contract_view_${hex().slice(0, 12)}`;
await call('POST', `/v1/operator/attributes/${viewAttribute}`, 201, {headers: operator, body: {type: 'string'}});
await call('POST', `/v1/operator/attributes/${viewAttribute}/access`, 201, {headers: operator, body: {readers: ['operator'], sources: ['importer']}});

async function sourceRecord(namespace: string) {
  const record = `contract-${hex()}`;
  const created = await plainJSON(api, 'POST', '/v1/profiles', 201, {
    headers: {...importer, 'Idempotency-Key': uuid()},
    body: {set: {display_name: 'Synthetic contract record', [viewAttribute]: 'synthetic'}, identifiers: [{namespace, value: record}]},
  });
  return {id: created.id as string, record};
}
const claim = (namespace: string, record: string, subject = customer.subject) =>
  ({namespace, record, customer_subject: subject, proof_method: 'synthetic_challenge', proof_reference: uuid()});

const linked = await sourceRecord('salesforce');
const issued = claim('salesforce', linked.record);
await call('POST', grantsPath, 401, {headers: importer, body: issued});
await call('POST', grantsPath, 401, {headers: customer.native, body: issued});
await call('POST', grantsPath, 415, {headers: {...issuer, 'Content-Type': 'text/plain'}, raw: 'synthetic'});
await call('POST', grantsPath, 400, {headers: issuer, body: {...issued, unknown: true}});
await call('POST', grantsPath, 422, {headers: issuer, body: {...issued, proof_method: 'email_match'}});
const grant = await call('POST', grantsPath, 201, {headers: issuer, body: issued});
await call('POST', grantsPath, 409, {headers: issuer, body: issued});

await call('GET', associationsPath, 401);
await call('GET', `${associationsPath}?limit=0`, 400, {headers: customer.native});
await call('GET', associationsPath, 200, {headers: customer.native});
await call('POST', associationsPath, 401, {body: {grant: grant.grant}});
await call('POST', associationsPath, 403, {headers: {Cookie: cookie, Origin: 'https://attacker.example'}, body: {grant: grant.grant}});
await call('POST', associationsPath, 415, {headers: {...customer.native, 'Content-Type': 'text/plain'}, raw: 'synthetic'});
await call('POST', associationsPath, 400, {headers: customer.native, body: {grant: grant.grant, unknown: true}});
await call('POST', associationsPath, 409, {headers: other.native, body: {grant: grant.grant}});
const association = await call('POST', associationsPath, 201, {headers: sameOrigin, body: {grant: grant.grant}});
const associationPath = `${associationsPath}/${association.id}`;
await call('POST', associationsPath, 409, {headers: customer.native, body: {grant: grant.grant}});
await call('GET', associationPath, 401);
await call('GET', associationPath, 404, {headers: other.native});
await call('GET', `${associationPath}?x=1`, 400, {headers: customer.native});
await call('GET', associationPath, 200, {headers: {Cookie: cookie}});
await call('DELETE', associationPath, 401);
await call('DELETE', associationPath, 404, {headers: other.native});
await call('DELETE', `${associationPath}?x=1`, 400, {headers: customer.native});

const operatorProfiles = '/v1/operator/profiles';
await call('GET', operatorProfiles, 401, {headers: customer.native});
await call('GET', operatorProfiles, 401, {headers: importer});
await call('GET', `${operatorProfiles}?limit=0`, 400, {headers: operator});
await call('GET', `${operatorProfiles}?limit=1`, 200, {headers: operator});
await call('GET', `${operatorProfiles}/${linked.id}`, 401);
await call('GET', `${operatorProfiles}/${uuid()}`, 404, {headers: operator});
await call('GET', `${operatorProfiles}/${linked.id}`, 200, {headers: operator});

const unifiedView = (id: string) => `/v1/operator/customers/${id}/unified-view`;
await call('GET', unifiedView(anchor.id), 401);
await call('GET', unifiedView(anchor.id), 401, {headers: customer.native});
await call('GET', unifiedView(anchor.id), 401, {headers: importer});
await call('GET', unifiedView(anchor.id), 401, {headers: issuer});
await call('GET', `${unifiedView(anchor.id)}?subject=${uuid()}`, 400, {headers: operator});
await call('GET', unifiedView(linked.id), 404, {headers: operator});
const view = await call('GET', unifiedView(anchor.id), 200, {headers: operator});
assert.equal(view.records[0].association_id, association.id);
assert.equal(view.attributes[0].path, viewAttribute);
assert.equal(view.attributes[0].values[0].record_profile_id, linked.id);
assert.deepEqual(await call('GET', unifiedView(otherAnchor.id), 200, {headers: operator}), {anchor_profile_id: otherAnchor.id, logic: view.logic, reader: 'operator', attributes: [], records: []});
assert.deepEqual(await call('GET', profilePath, 200, {headers: customer.native}), anchor);

const consent = '/v1/operator/consent';
const purposes = `${consent}/purposes`;
const purposeName = `contract_${hex()}`;
await call('POST', purposes, 401, {headers: customer.native, body: {name: purposeName}});
await call('POST', purposes, 415, {headers: {...operator, 'Content-Type': 'text/plain'}, raw: 'synthetic'});
await call('POST', purposes, 400, {headers: operator, body: {name: purposeName, unknown: true}});
await call('POST', purposes, 422, {headers: operator, body: {name: 'Not Valid'}});
const purpose = await call('POST', purposes, 201, {headers: operator, body: {name: purposeName}});
await call('POST', purposes, 409, {headers: operator, body: {name: purposeName}});

const draftBody = {
  lawful_basis: 'consent', notice_reference: 'contract-notice', explanation: 'Synthetic contract explanation.', jurisdiction: 'DE', locale: 'de-DE',
  channel: 'email', retention_reference: 'contract-retention',
};
const policies = `${consent}/policies/${purpose.id}`;
await call('GET', policies, 401);
await call('GET', `${consent}/policies/${uuid()}`, 404, {headers: operator});
await call('POST', policies, 401, {body: draftBody});
await call('POST', `${consent}/policies/${uuid()}`, 404, {headers: operator, body: draftBody});
await call('POST', policies, 415, {headers: {...operator, 'Content-Type': 'text/plain'}, raw: 'synthetic'});
await call('POST', policies, 400, {headers: operator, body: {...draftBody, unknown: true}});
await call('POST', policies, 422, {headers: operator, body: {...draftBody, jurisdiction: 'germany'}});
const draft = await call('POST', policies, 201, {headers: operator, body: draftBody});
await call('GET', policies, 200, {headers: operator});

const state = (id: string) => `${consent}/policy-versions/${id}/state`;
await call('POST', state(draft.id), 401, {body: {state: 'active'}});
await call('POST', state(uuid()), 404, {headers: operator, body: {state: 'active'}});
await call('POST', state(draft.id), 415, {headers: {...operator, 'Content-Type': 'text/plain'}, raw: 'synthetic'});
await call('POST', state(draft.id), 400, {headers: operator, body: {state: 'active', unknown: true}});
await call('POST', state(draft.id), 422, {headers: operator, body: {state: 'draft'}});
await call('POST', state(draft.id), 409, {headers: operator, body: {state: 'retired'}});
await call('POST', state(draft.id), 200, {headers: operator, body: {state: 'active'}});

const evidence = (decision = 'withdraw') => ({
  policy_version_id: draft.id, decision, occurred_at: new Date().toISOString(),
  proof_channel: 'contract_test', proof_reference: 'contract-proof',
});
const [{record_epoch: recordEpoch}] = await database`SELECT record_epoch FROM external_identifiers WHERE value = ${linked.record}`;
for (const [path, missing] of [
  [`${consent}/evidence/customer/${anchor.id}`, `${consent}/evidence/customer/${uuid()}`],
  [`${consent}/evidence/record/${recordEpoch}`, `${consent}/evidence/record/${uuid()}`],
]) {
  const key = uuid();
  const body = evidence();
  await call('POST', path, 401, {headers: {'Idempotency-Key': key}, body});
  await call('POST', missing, 404, {headers: {...operator, 'Idempotency-Key': key}, body});
  await call('POST', path, 415, {headers: {...operator, 'Idempotency-Key': key, 'Content-Type': 'text/plain'}, raw: 'synthetic'});
  await call('POST', path, 400, {headers: {...operator, 'Idempotency-Key': key}, body: {...body, unknown: true}});
  await call('POST', path, 422, {headers: {...operator, 'Idempotency-Key': key}, body: {...body, decision: 'maybe'}});
  await call('POST', path, 201, {headers: {...operator, 'Idempotency-Key': key}, body});
  await call('POST', path, 200, {headers: {...operator, 'Idempotency-Key': key}, body});
  await call('POST', path, 409, {headers: {...operator, 'Idempotency-Key': key}, body: {...body, decision: 'deny'}});
}

const effective = `${consent}/effective/${anchor.id}/${purpose.id}`;
await call('GET', effective, 401);
await call('GET', `${consent}/effective/${uuid()}/${purpose.id}`, 404, {headers: operator});
await call('GET', effective, 200, {headers: operator});

await call('DELETE', associationPath, 204, {headers: sameOrigin});
await call('DELETE', associationPath, 204, {headers: customer.native});
await call('GET', associationPath, 404, {headers: customer.native});
const revocable = await sourceRecord('erp');
const pending = await call('POST', grantsPath, 201, {headers: issuer, body: claim('erp', revocable.record, other.subject)});
await call('DELETE', `${grantsPath}/${pending.id}`, 401, {headers: importer});
await call('DELETE', `${grantsPath}/${pending.id}?x=1`, 400, {headers: issuer});
await call('DELETE', `${grantsPath}/${uuid()}`, 404, {headers: issuer});
await call('DELETE', `${grantsPath}/${pending.id}`, 204, {headers: issuer});

const ownConsent = '/v1/customer/consent';
const ownHistory = `${ownConsent}/history`;
const decisions = `${ownConsent}/decisions`;
async function purposeState(headers: Headers, id: string) {
  for (let cursor = ''; ;) {
    const page = await call('GET', `${ownConsent}?limit=100${cursor && `&cursor=${cursor}`}`, 200, {headers});
    const item = page.items.find((candidate: any) => candidate.purpose_id === id);
    if (item) return item;
    assert.ok(page.next_cursor, 'The active purpose was missing from the customer consent state');
    cursor = page.next_cursor;
  }
}
const decide = (decision: string, policy = draft.id) => ({policy_version_id: policy, decision});
const write = (key: string, revision?: number): Headers => ({
  ...other.native, 'Idempotency-Key': key, ...(revision === undefined ? {} : {'If-Match': `"${revision}"`}),
});
const unprovisioned = await verifiedCustomer();
await call('GET', ownConsent, 404, {headers: unprovisioned.native});
await call('GET', ownHistory, 404, {headers: unprovisioned.native});
await call('POST', decisions, 409, {headers: {...unprovisioned.native, 'Idempotency-Key': uuid(), 'If-Match': '"1"'}, body: decide('grant')});
await call('GET', ownConsent, 401);
await call('GET', ownConsent, 401, {headers: importer});
await call('GET', ownConsent, 401, {headers: operator});
await call('GET', `${ownConsent}?limit=0`, 400, {headers: other.native});
await call('GET', ownHistory, 401);
await call('GET', `${ownHistory}?limit=0`, 400, {headers: other.native});
const before = await purposeState(other.native, purpose.id);
assert.equal(before.policy.explanation, 'Synthetic contract explanation.');
assert.equal(before.policy.version, draft.version);
await call('POST', decisions, 401, {headers: {'Idempotency-Key': uuid()}, body: decide('grant')});
await call('POST', decisions, 401, {headers: {...operator, 'Idempotency-Key': uuid()}, body: decide('grant')});
await call('POST', decisions, 401, {headers: {...importer, 'Idempotency-Key': uuid()}, body: decide('grant')});
await call('POST', decisions, 403, {headers: {Cookie: cookie, Origin: 'https://attacker.example', 'Idempotency-Key': uuid()}, body: decide('grant')});
await call('POST', decisions, 415, {headers: {...write(uuid()), 'Content-Type': 'text/plain'}, raw: 'synthetic'});
await call('POST', decisions, 400, {headers: write(uuid()), body: {...decide('grant'), actor_kind: 'operator'}});
await call('POST', decisions, 422, {headers: write(uuid()), body: decide('object')});
await call('POST', decisions, 422, {headers: write(uuid()), body: decide('grant', 'not-a-policy')});
await call('POST', decisions, 422, {headers: write(uuid(), before.revision), body: decide('grant', uuid())});
await call('POST', decisions, 422, {headers: other.native, body: decide('grant')});
await call('POST', decisions, 428, {headers: write(uuid()), body: decide('grant')});
await call('POST', decisions, 400, {headers: {...write(uuid()), 'If-Match': 'stale'}, body: decide('grant')});
await call('POST', decisions, 412, {headers: write(uuid(), before.revision + 1), body: decide('grant')});
const unpublished = await call('POST', policies, 201, {headers: operator, body: draftBody});
await call('POST', decisions, 409, {headers: write(uuid(), before.revision), body: decide('grant', unpublished.id)});
const key = uuid();
const receipt = await call('POST', decisions, 201, {headers: write(key, before.revision), body: decide('grant')});
assert.deepEqual({...await call('POST', decisions, 200, {headers: write(key, before.revision), body: decide('grant')}), created: true}, receipt);
await call('POST', decisions, 409, {headers: write(key), body: decide('withdraw')});
const permission = (profile: string) => `${consent}/effective/${profile}/${purpose.id}`;
assert.equal((await call('GET', permission(otherAnchor.id), 200, {headers: operator})).outcome, 'permitted');
await call('POST', decisions, 201, {headers: write(uuid()), body: decide('withdraw')});
assert.equal((await call('GET', permission(otherAnchor.id), 200, {headers: operator})).outcome, 'blocked');
const history = await call('GET', `${ownHistory}?limit=2`, 200, {headers: other.native});
assert.deepEqual(history.items.map((item: any) => [item.decision, item.actor_kind]), [['withdraw', 'customer'], ['grant', 'customer']]);
const browserState = await purposeState({Cookie: cookie}, purpose.id);
await call('POST', decisions, 201, {headers: {...sameOrigin, 'Idempotency-Key': uuid(), 'If-Match': `"${browserState.revision}"`}, body: decide('grant')});
await call('GET', ownHistory, 200, {headers: {Cookie: cookie}});

const sourceEpoch = (source: string) => `/v1/operator/sources/${source}/epoch`;
await call('GET', sourceEpoch('console'), 401, {headers: importer});
await call('GET', sourceEpoch('no_such_source'), 404, {headers: operator});
const {epoch: currentEpoch} = await call('GET', sourceEpoch('console'), 200, {headers: operator});
const expectEpoch = {...operator, 'If-Match': `"${currentEpoch}"`};
await call('POST', sourceEpoch('console'), 401, {headers: customer.native});
await call('POST', sourceEpoch('console'), 401, {headers: importer});
await call('POST', sourceEpoch('no_such_source'), 404, {headers: expectEpoch});
await call('POST', sourceEpoch('console'), 400, {headers: expectEpoch, body: {}});
await call('POST', sourceEpoch('console'), 428, {headers: operator});
await call('POST', sourceEpoch('console'), 412, {headers: {...operator, 'If-Match': `"${currentEpoch + 1}"`}});
assert.equal((await call('POST', sourceEpoch('console'), 200, {headers: expectEpoch})).epoch, currentEpoch + 1);

// A fresh name per run, because definitions are immutable and the stack's database is reused.
const attributeDefinitions = '/v1/operator/attributes';
const attributePath = `${attributeDefinitions}/contract_${hex().slice(0, 12)}`;
await call('GET', attributeDefinitions, 401, {headers: importer});
await call('GET', attributePath, 404, {headers: operator});
await call('POST', attributePath, 401, {headers: importer, body: {type: 'enum', values: ['gold']}});
await call('POST', attributePath, 400, {headers: operator, body: {type: 'enum', values: ['gold'], unknown: true}});
await call('POST', attributePath, 422, {headers: operator, body: {type: 'enum'}});
assert.equal((await call('POST', attributePath, 201, {headers: operator, body: {type: 'enum', values: ['gold']}})).revision, 1);
await call('POST', attributePath, 428, {headers: operator, body: {type: 'enum', values: ['gold', 'silver']}});
await call('POST', attributePath, 412, {headers: {...operator, 'If-Match': '"2"'}, body: {type: 'enum', values: ['gold', 'silver']}});
await call('POST', attributePath, 409, {headers: {...operator, 'If-Match': '"1"'}, body: {type: 'string'}});
await call('POST', attributePath, 201, {headers: {...operator, 'If-Match': '"1"'}, body: {type: 'enum', values: ['gold', 'silver']}});
assert.equal((await call('GET', attributePath, 200, {headers: operator})).items.length, 2);
assert.ok((await call('GET', attributeDefinitions, 200, {headers: operator})).items.some((item: any) => attributePath.endsWith(item.path)));
const accessPath = `${attributePath}/access`;
const accessGrant = {readers: ['operator', 'segmentation'], sources: ['importer']};
await call('GET', `${attributeDefinitions}/contract_undefined_${hex().slice(0, 8)}/access`, 404, {headers: operator});
await call('POST', accessPath, 401, {headers: importer, body: accessGrant});
await call('POST', accessPath, 400, {headers: operator, body: {...accessGrant, unknown: true}});
await call('POST', accessPath, 422, {headers: operator, body: {readers: ['customer'], sources: ['importer']}});
assert.deepEqual((await call('GET', accessPath, 200, {headers: operator})).items, []);
assert.equal((await call('POST', accessPath, 201, {headers: operator, body: accessGrant})).revision, 1);
await call('POST', accessPath, 428, {headers: operator, body: accessGrant});
await call('POST', accessPath, 412, {headers: {...operator, 'If-Match': '"2"'}, body: accessGrant});
await call('POST', accessPath, 201, {headers: {...operator, 'If-Match': '"1"'}, body: {readers: [], sources: []}});
assert.equal((await call('GET', accessPath, 200, {headers: operator})).items.length, 2);

// Segments: a fresh rule per run over a segmentation-granted attribute, gated on the contract purpose.
const segmentAttribute = `contract_seg_${hex().slice(0, 8)}`;
await call('POST', `${attributeDefinitions}/${segmentAttribute}`, 201, {headers: operator, body: {type: 'integer'}});
await call('POST', `${attributeDefinitions}/${segmentAttribute}/access`, 201, {headers: operator, body: {readers: ['segmentation'], sources: ['importer']}});
const segments = '/v1/operator/segments';
const segmentPath = `${segments}/contract_${hex().slice(0, 12)}`;
const segmentRule = {purpose_id: purpose.id, match: 'all', conditions: [{attribute: segmentAttribute, operator: 'missing'}]};
await call('GET', segments, 401, {headers: importer});
await call('GET', segmentPath, 404, {headers: operator});
await call('GET', `${segmentPath}/preview`, 404, {headers: operator});
await call('POST', segmentPath, 401, {headers: importer, body: segmentRule});
await call('POST', segmentPath, 400, {headers: operator, body: {...segmentRule, sql: 'SELECT 1'}});
await call('POST', segmentPath, 422, {headers: operator, body: {...segmentRule, conditions: [{attribute: segmentAttribute, operator: 'gt', value: 'x'}]}});
assert.equal((await call('POST', segmentPath, 201, {headers: operator, body: segmentRule})).revision, 1);
await call('POST', segmentPath, 428, {headers: operator, body: segmentRule});
await call('POST', segmentPath, 412, {headers: {...operator, 'If-Match': '"2"'}, body: segmentRule});
await call('POST', segmentPath, 201, {headers: {...operator, 'If-Match': '"1"'}, body: {...segmentRule, match: 'any'}});
assert.equal((await call('GET', segmentPath, 200, {headers: operator})).items.length, 2);
assert.ok((await call('GET', segments, 200, {headers: operator})).items.some((item: any) => segmentPath.endsWith(item.name)));
await call('GET', `${segmentPath}/preview`, 401, {headers: customer.native});
await call('GET', `${segmentPath}/preview?limit=0`, 400, {headers: operator});
const segmentPreview = await call('GET', `${segmentPath}/preview?limit=100`, 200, {headers: operator});
assert.equal(segmentPreview.revision, 2);
// No source holds the new attribute, so membership follows consent: the browser grant counts, the withdrawal does not.
const sampled = segmentPreview.sample.map((member: any) => member.anchor_profile_id);
assert.ok(sampled.includes(anchor.id) && !sampled.includes(otherAnchor.id), 'segment membership must follow effective consent');

// Snapshots: two snapshots of the same segment compare to no change; another segment cannot be compared.
const snapshots = `${segmentPath}/snapshots`;
await call('POST', snapshots, 401, {headers: importer});
await call('POST', `${segments}/contract_unknown_${hex().slice(0, 8)}/snapshots`, 404, {headers: operator});
const firstSnapshot = await call('POST', snapshots, 201, {headers: operator});
const secondSnapshot = await call('POST', snapshots, 201, {headers: operator});
await call('GET', `${segments}/contract_unknown_${hex().slice(0, 8)}/snapshots`, 404, {headers: operator});
assert.ok((await call('GET', snapshots, 200, {headers: operator})).items.some((item: any) => item.id === firstSnapshot.id));
const snapshotChanges = (id: string, since: string) => `/v1/operator/segment-snapshots/${id}/changes?since=${since}`;
await call('GET', snapshotChanges(secondSnapshot.id, uuid()), 404, {headers: operator});
await call('GET', `/v1/operator/segment-snapshots/${secondSnapshot.id}/changes`, 400, {headers: operator});
const unchanged = await call('GET', snapshotChanges(secondSnapshot.id, firstSnapshot.id), 200, {headers: operator});
assert.deepEqual([unchanged.joined, unchanged.left], [[], []]);
await call('GET', `${snapshotChanges(secondSnapshot.id, firstSnapshot.id)}&limit=1&joined_cursor=${uuid()}&left_cursor=${uuid()}`, 200, {headers: operator});
await call('GET', `${snapshotChanges(secondSnapshot.id, firstSnapshot.id)}&limit=0`, 400, {headers: operator});
await call('GET', `${snapshotChanges(secondSnapshot.id, firstSnapshot.id)}&joined_cursor=not-a-uuid`, 400, {headers: operator});

// Activation: the preview's member is queued for the synthetic webhook and delivered by the dispatcher.
const destinations = '/v1/operator/destinations';
const destinationName = `contract_${hex().slice(0, 12)}`;
await call('GET', destinations, 401, {headers: importer});
await call('POST', `${destinations}/${destinationName}`, 401, {headers: importer, body: {url: 'http://webhook-sink:8090/hook/contract'}});
await call('POST', `${destinations}/${destinationName}`, 400, {headers: operator, body: {url: 'http://webhook-sink:8090/hook/contract', extra: true}});
await call('POST', `${destinations}/${destinationName}`, 422, {headers: operator, body: {url: 'http://169.254.169.254:80/latest'}});
const registered = await call('POST', `${destinations}/${destinationName}`, 201, {headers: operator, body: {url: 'http://webhook-sink:8090/hook/contract'}});
assert.match(registered.signing_secret, /^[0-9a-f]{64}$/);
await call('POST', `${destinations}/${destinationName}`, 409, {headers: operator, body: {url: 'http://webhook-sink:8090/hook/contract'}});
const listedDestinations = await call('GET', destinations, 200, {headers: operator});
assert.ok(listedDestinations.items.every((item: any) => item.signing_secret === undefined));
const activations = `${segmentPath}/activations`;
await call('POST', activations, 401, {headers: importer, body: {destination: destinationName}});
await call('POST', `${segments}/contract_unknown_${hex().slice(0, 8)}/activations`, 404, {headers: operator, body: {destination: destinationName}});
await call('POST', activations, 422, {headers: operator, body: {destination: `missing_${hex().slice(0, 8)}`}});
const activation = await call('POST', activations, 201, {headers: operator, body: {destination: destinationName}});
assert.equal(activation.queued, segmentPreview.members);
const activationPath = `/v1/operator/activations/${activation.id}`;
await call('GET', `/v1/operator/activations/${uuid()}`, 404, {headers: operator});
await call('GET', activationPath, 401, {headers: customer.native});
let settled: any;
for (let tries = 0; tries < 30; tries++) {
  settled = await call('GET', activationPath, 200, {headers: operator});
  if (settled.deliveries.every((delivery: any) => delivery.state !== 'pending')) break;
  await new Promise(resolve => setTimeout(resolve, 1000));
}
const firstDelivery = await call('GET', `${activationPath}?limit=1`, 200, {headers: operator});
assert.ok(firstDelivery.deliveries.length <= 1 && (firstDelivery.next_cursor === undefined) === (activation.queued <= 1));
await call('GET', `${activationPath}?limit=0`, 400, {headers: operator});
await call('GET', `${activationPath}?cursor=not-a-uuid`, 400, {headers: operator});
const anchorDelivery = settled.deliveries.find((delivery: any) => delivery.anchor_profile_id === anchor.id);
assert.equal(anchorDelivery?.state, 'delivered', 'the consenting member must be delivered');
assert.equal(anchorDelivery.receipts.at(-1).status_code, 200);

await call('GET', '/v1/operator/activations', 401, {headers: importer});
assert.ok((await call('GET', '/v1/operator/activations', 200, {headers: operator})).items.some((item: any) => item.id === activation.id));
await call('GET', '/v1/operator/audience-metrics', 401, {headers: importer});
assert.ok((await call('GET', '/v1/operator/audience-metrics', 200, {headers: operator})).activations >= 1);
const redrive = `${activationPath}/redrive`;
await call('POST', redrive, 401, {headers: importer});
await call('POST', `/v1/operator/activations/${uuid()}/redrive`, 404, {headers: operator});
assert.equal((await call('POST', redrive, 200, {headers: operator})).requeued, settled.counts.failed);

// Audience jobs: the API's own workers snapshot and activate the same segment outside the request.
const jobKey = () => `contract-${hex()}`;
const snapshotJobs = `${segmentPath}/snapshot-jobs`;
const activationJobs = `${segmentPath}/activation-jobs`;
const audienceJob = (id: string) => `/v1/operator/audience-jobs/${id}`;
await call('POST', snapshotJobs, 401, {headers: {...importer, 'Idempotency-Key': jobKey()}});
await call('POST', snapshotJobs, 400, {headers: operator});
await call('POST', `${segments}/contract_unknown_${hex().slice(0, 8)}/snapshot-jobs`, 404, {headers: {...operator, 'Idempotency-Key': jobKey()}});
const snapshotJobHeaders = {...operator, 'Idempotency-Key': jobKey()};
const queuedSnapshotJob = await call('POST', snapshotJobs, 202, {headers: snapshotJobHeaders});
assert.equal((await call('POST', snapshotJobs, 200, {headers: snapshotJobHeaders})).id, queuedSnapshotJob.id);
await call('POST', activationJobs, 409, {headers: snapshotJobHeaders, body: {destination: destinationName}});
await call('POST', activationJobs, 401, {headers: {...importer, 'Idempotency-Key': jobKey()}, body: {destination: destinationName}});
await call('POST', activationJobs, 422, {headers: {...operator, 'Idempotency-Key': jobKey()}, body: {destination: `missing_${hex().slice(0, 8)}`}});
const queuedActivationJob = await call('POST', activationJobs, 202, {headers: {...operator, 'Idempotency-Key': jobKey()}, body: {destination: destinationName}});
const finishedJob = async (id: string) => {
  let job: any;
  for (let tries = 0; tries < 60; tries++) {
    job = await call('GET', audienceJob(id), 200, {headers: operator});
    if (job.state !== 'queued' && job.state !== 'running') break;
    await new Promise(resolve => setTimeout(resolve, 1000));
  }
  return job;
};
const snapshotJob = await finishedJob(queuedSnapshotJob.id);
const membersNow = (await call('GET', `${segmentPath}/preview`, 200, {headers: operator})).members;
assert.deepEqual([snapshotJob.state, snapshotJob.members, snapshotJob.attempts], ['completed', membersNow, 1]);
const snapshotMembers = `/v1/operator/segment-snapshots/${snapshotJob.snapshot_id}/members`;
const memberPage = await call('GET', `${snapshotMembers}?limit=1`, 200, {headers: operator});
assert.ok(memberPage.members.length <= 1 && memberPage.snapshot.members === membersNow);
await call('GET', `${snapshotMembers}?limit=0`, 400, {headers: operator});
await call('GET', snapshotMembers, 401, {headers: importer});
await call('GET', `/v1/operator/segment-snapshots/${uuid()}/members`, 404, {headers: operator});
const activationJob = await finishedJob(queuedActivationJob.id);
assert.equal(activationJob.state, 'completed');
assert.equal((await call('GET', `/v1/operator/activations/${activationJob.activation_id}`, 200, {headers: operator})).id, activationJob.activation_id);
await call('GET', audienceJob(uuid()), 404, {headers: operator});
await call('GET', audienceJob(snapshotJob.id), 401, {headers: importer});
await call('POST', `${audienceJob(snapshotJob.id)}/cancel`, 409, {headers: operator});
await call('POST', `${audienceJob(uuid())}/cancel`, 404, {headers: operator});
await call('POST', `${audienceJob(snapshotJob.id)}/cancel`, 401, {headers: importer});
await call('GET', '/v1/operator/audience-jobs', 401, {headers: importer});
assert.ok((await call('GET', '/v1/operator/audience-jobs', 200, {headers: operator})).items.some((item: any) => item.id === snapshotJob.id));

// Match candidates: a source record is compared without disclosing values; anchors are not source records.
const matchCandidates = (id: string) => `/v1/operator/profiles/${id}/match-candidates`;
await call('GET', matchCandidates(linked.id), 401, {headers: importer});
await call('GET', matchCandidates(anchor.id), 404, {headers: operator});
await call('GET', `${matchCandidates(linked.id)}?x=1`, 400, {headers: operator});
const proposals = await call('GET', matchCandidates(linked.id), 200, {headers: operator});
assert.equal(proposals.source, 'importer');
// Grant matching on two fixed signal paths (idempotently: the stack's database is reused) and review a real pair.
for (const path of ['email', 'family_name']) {
  if (!(await call('GET', attributeDefinitions, 200, {headers: operator})).items.some((item: any) => item.path === path)) {
    await call('POST', `${attributeDefinitions}/${path}`, 201, {headers: operator, body: {type: 'string'}});
  }
  // Merge into the current policy so other readers and sources keep their access.
  const policies = (await call('GET', `${attributeDefinitions}/${path}/access`, 200, {headers: operator})).items;
  const current = policies.at(-1) ?? {readers: [], sources: []};
  if (!current.readers.includes('matching') || !current.sources.includes('importer')) {
    await call('POST', `${attributeDefinitions}/${path}/access`, 201, {headers: policies.length ? {...operator, 'If-Match': `"${policies.length}"`} : operator,
      body: {readers: [...new Set([...current.readers, 'matching'])], sources: [...new Set([...current.sources, 'importer'])]}});
  }
}
const pairEmail = `pair-${hex().slice(0, 12)}@example.com`;
const pair: string[] = [];
for (const namespace of ['crm', 'erp']) {
  pair.push((await plainJSON(api, 'POST', '/v1/profiles', 201, {headers: {...importer, 'Idempotency-Key': uuid()},
    body: {set: {email: pairEmail, family_name: 'Contract'}, identifiers: [{namespace, value: `pair-${hex()}`}]}})).id as string);
}
const reviews = `/v1/operator/profiles/${pair[0]}/match-reviews`;
await call('POST', reviews, 401, {headers: importer, body: {candidate_profile_id: pair[1], decision: 'same_person'}});
await call('POST', reviews, 409, {headers: operator, body: {candidate_profile_id: linked.id, decision: 'same_person'}});
const recorded = await call('POST', reviews, 201, {headers: operator, body: {candidate_profile_id: pair[1], decision: 'same_person'}});
assert.equal(recorded.class, 'probable');
await call('GET', `/v1/operator/profiles/${pair[1]}/match-reviews`, 401, {headers: importer});
assert.equal((await call('GET', `/v1/operator/profiles/${pair[1]}/match-reviews`, 200, {headers: operator})).items[0].id, recorded.id);

// Customer-owned preferences: versioned replacement by the session customer only.
const preferences = '/v1/customer/profile/preferences';
await call('GET', preferences, 401, {headers: operator});
const prefsBefore = await call('GET', preferences, 200, {headers: customer.native});
const firstWrite = prefsBefore.version === 0 ? {} : {'If-Match': `"${prefsBefore.version}"`};
await call('PUT', preferences, 422, {headers: {...customer.native, ...firstWrite}, body: {timezone: 'Mars/Olympus'}});
const prefsSaved = await call('PUT', preferences, 200, {headers: {...customer.native, ...firstWrite}, body: {display_name: 'Contract Customer', preferred_language: 'de-de', timezone: 'Europe/Berlin'}});
assert.equal(prefsSaved.preferred_language, 'de-DE');
await call('PUT', preferences, 428, {headers: customer.native, body: {timezone: 'UTC'}});
await call('PUT', preferences, 412, {headers: {...customer.native, 'If-Match': `"${prefsSaved.version + 1}"`}, body: {timezone: 'UTC'}});

// Operator writes are audited without bodies; the development bearer is recorded as such.
await call('GET', '/v1/operator/audit', 401, {headers: importer});
const auditTrail = await call('GET', '/v1/operator/audit', 200, {headers: operator});
assert.ok(auditTrail.items.some((entry: any) => entry.actor === 'development-bearer' && entry.method === 'POST'));

// OAuth applications in Hydra (ADR 0012): registration policy, secret once, list without secrets, delete.
const applications = '/v1/operator/applications';
await call('GET', applications, 401, {headers: importer});
await call('POST', applications, 422, {headers: operator, body: {name: 'Bad', type: 'public', redirect_uris: ['http://example.test/cb'], scopes: ['openid']}});
const confidential = await call('POST', applications, 201, {headers: operator,
  body: {name: 'Contract App', type: 'confidential', redirect_uris: ['https://app.example.test/callback'], scopes: ['openid', 'profile.read']}});
assert.ok(confidential.client_secret, 'a confidential application receives its secret once');
const registeredApps = await call('GET', applications, 200, {headers: operator});
assert.ok(registeredApps.items.some((item: any) => item.client_id === confidential.client_id && item.client_secret === undefined));
await call('DELETE', `${applications}/${confidential.client_id}`, 204, {headers: operator});
await call('DELETE', `${applications}/${confidential.client_id}`, 404, {headers: operator});

// Every operation must have every documented success and at least one documented error checked live.
for (const entry of operations.values()) {
  for (const method of ['get', 'put', 'post', 'delete', 'patch']) {
    const operation = entry.operations[method];
    if (!operation) continue;
    const seen = exercised.get(operation.operationId) ?? new Set<number>();
    for (const status of Object.keys(operation.responses).map(Number).filter(code => code < 400)) {
      assert.ok(seen.has(status), `${operation.operationId}: success ${status} was not exercised`);
    }
    assert.ok([...seen].some(code => code >= 400), `${operation.operationId}: no error response was exercised`);
  }
}
await database.close();
console.log('PASS: every customer, linking and operator operation matched its live contract');
