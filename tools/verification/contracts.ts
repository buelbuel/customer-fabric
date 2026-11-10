import assert from 'node:assert/strict';
import { ajv, contract } from './openapi.ts';

if (!process.env.CF_CONTRACT_BASE_URL) {
  if (process.env.CF_REQUIRE_LIVE_CONTRACTS === '1') throw new Error('CF_CONTRACT_BASE_URL is required');
  console.log('Live contract checks not requested.');
  process.exit(0);
}
const base = process.env.CF_CONTRACT_BASE_URL;
// Bounded readiness wait for freshly started Compose/CI services. Never substitutes responses.
for (let attempt = 0; ; attempt++) {
  try {
    const ready = await fetch(base + '/healthz', {signal: AbortSignal.timeout(2000), redirect: 'error'});
    if (ready.ok) break;
  } catch {}
  if (attempt === 29) throw new Error('Real backend did not become healthy');
  await Bun.sleep(1000);
}
const tokens = JSON.parse(process.env.CF_DEV_TOKENS || '{}');
assert.ok(tokens.console && tokens.importer, 'console/importer credentials required');
const profilePath = '/v1/profiles/{id}';
async function request(method: string, path: string, expected: number, body?: object, token = tokens.console, key = crypto.randomUUID(), etag?: string) {
  const route = path.split('?')[0].startsWith('/v1/profiles/') ? profilePath : path.split('?')[0].replace(/^\/v1\/imports\/[0-9a-f-]{36}/, '/v1/imports/{id}');
  const operation = contract.paths[route][method.toLowerCase()];
  const headers: Record<string,string> = {Authorization:`Bearer ${token}`, 'Content-Type':'application/json'};
  if (method !== 'GET') headers['Idempotency-Key'] = key;
  if (etag) headers['If-Match'] = etag;
  const response = await fetch(base + path,{method,headers,body:body ? JSON.stringify(body):undefined,redirect:'error'});
  assert.equal(response.status,expected,`${method} ${route}: unexpected status`);
  const actual = await response.json();
  const definition = operation.responses[String(expected)] || operation.responses.default;
  const media = expected >=400 ? 'application/problem+json' : 'application/json';
  assert.ok(response.headers.get('content-type')?.startsWith(media));
  const validate = ajv.compile(definition.content[media].schema);
  assert.ok(validate(actual), `${method} ${route}: ${ajv.errorsText(validate.errors)}`);
  assert.ok(response.headers.get('cache-control')?.includes('no-store'));
  if (definition.headers?.ETag) assert.match(response.headers.get('etag') || '', /^"[1-9][0-9]*"$/);
  console.log(`PASS: ${method} ${route} → ${expected}`);
  return {body:actual as any, response};
}
await request('GET','/healthz',200);
await request('GET','/v1/profiles',401,undefined,'invalid');
const suffix=crypto.randomUUID();
const payload={set:{display_name:'Synthetic contract verification'},identifiers:[{namespace:'contract',value:suffix}]};
const createKey=crypto.randomUUID();
const created=await request('POST','/v1/profiles',201,payload,tokens.console,createKey);
assert.equal(created.response.headers.get('location'),`/v1/profiles/${created.body.id}`);
assert.deepEqual((await request('POST','/v1/profiles',201,payload,tokens.console,createKey)).body,created.body);
const path=`/v1/profiles/${created.body.id}`;
await request('GET',path,200);
await request('GET','/v1/profiles?limit=1',200);
await request('PATCH',path,409,{set:{display_name:'Conflicting source'}},tokens.importer,crypto.randomUUID(),'"1"');
await request('PATCH',path,428,{set:{display_name:'Missing version'}});
await request('PATCH',path,422,{set:{display_name:[]}},tokens.console,crypto.randomUUID(),'"1"');
await request('PATCH',path,200,{set:{display_name:'Synthetic updated'}},tokens.console,crypto.randomUUID(),'"1"');
await request('PATCH',path,412,{set:{display_name:'Stale'}},tokens.console,crypto.randomUUID(),'"1"');
await request('GET',`/v1/profiles/${crypto.randomUUID()}`,404);

const eventRecord = `contract-event-${suffix}`;
const eventProfile = await request('POST','/v1/profiles',201,{set:{display_name:'Synthetic event record'},identifiers:[{namespace:'contract',value:eventRecord}]});
const sourceEvent = (eventId: string, sequence: number, set: object = {status:'active'}) => ({
  event_id:eventId, type:'record.updated.v1', record:{namespace:'contract',value:eventRecord}, source_epoch:1, sequence,
  occurred_at:new Date(0).toISOString(), set,
});
const events = '/v1/source-events';
const applied = sourceEvent(`${suffix}-a`,5);
await request('POST',events,401,applied,'invalid');
await request('POST',events,400,{...applied,customer_subject:crypto.randomUUID()});
await request('POST',events,422,{...applied,sequence:0});
await request('POST',events,404,applied,tokens.importer);
const receipt = await request('POST',events,201,applied);
assert.equal(receipt.body.outcome,'applied');
assert.equal(receipt.body.profile_id,eventProfile.body.id);
assert.deepEqual((await request('POST',events,200,applied)).body,receipt.body);
await request('POST',events,409,{...applied,set:{status:'different'}});
await request('POST',events,409,{...sourceEvent(`${suffix}-e`,6),source_epoch:99});
assert.equal((await request('POST',events,201,sourceEvent(`${suffix}-s`,4))).body.outcome,'stale');

// Imports: an ordered NDJSON batch, a refused out-of-order batch, and completion.
const imported = await request('POST','/v1/imports',201,{max_errors:5});
await request('POST','/v1/imports',422,{max_errors:-1});
await request('GET',`/v1/imports/${crypto.randomUUID()}`,404);
await request('GET',`/v1/imports/${imported.body.id}`,404,undefined,tokens.importer);
async function importRows(offset: number, rows: object[], expected: number) {
  const response = await fetch(`${base}/v1/imports/${imported.body.id}/rows`,{method:'POST',redirect:'error',body:rows.map(r => JSON.stringify(r)).join('\n'),
    headers:{Authorization:`Bearer ${tokens.console}`,'Content-Type':'application/x-ndjson','Import-Offset':String(offset)}});
  assert.equal(response.status,expected,`POST /v1/imports/{id}/rows: unexpected status`);
  const definition = contract.paths['/v1/imports/{id}/rows'].post.responses[String(expected)];
  const media = expected >= 400 ? 'application/problem+json' : 'application/json';
  const validate = ajv.compile(definition.content[media].schema);
  const body = await response.json();
  assert.ok(validate(body), ajv.errorsText(validate.errors));
  console.log(`PASS: POST /v1/imports/{id}/rows → ${expected}`);
  return body;
}
const importedRecord = {namespace:'contract',value:`import-${suffix}`};
const batch = await importRows(1,[{record:importedRecord,set:{display_name:'Synthetic import'}},{record:importedRecord,set:'invalid'}],200);
assert.deepEqual([batch.checkpoint,batch.created,batch.quarantined],[2,1,1]);
await importRows(1,[{record:importedRecord,set:{display_name:'Repeat'}}],409);
assert.equal((await request('POST',`/v1/imports/${imported.body.id}/complete`,200)).body.state,'completed');
await request('POST',`/v1/imports/${imported.body.id}/complete`,409);
