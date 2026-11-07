import SwaggerParser from '@apidevtools/swagger-parser';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import { readFile } from 'node:fs/promises';
import assert from 'node:assert/strict';

const root = new URL('../../', import.meta.url);
const contract: any = await SwaggerParser.validate(new URL('contracts/openapi/customer-fabric.yaml', root).pathname, {resolve:{http:false}});
assert.equal(contract.openapi, '3.1.0');
const ajv = new Ajv2020({allErrors:true, strict:false, validateFormats:true});
addFormats(ajv);
for (const schema of Object.values(contract.components.schemas)) ajv.compile(schema as object);
for (const event of ['created','updated']) {
  const schema = JSON.parse(await readFile(new URL(`contracts/events/profile.${event}.v1.schema.json`, root),'utf8'));
  ajv.compile(schema);
}
console.log('PASS: OpenAPI 3.1 document, resolved references, component schemas and outbox schemas');

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
  const route = path.split('?')[0].startsWith('/v1/profiles/') ? profilePath : path.split('?')[0];
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
