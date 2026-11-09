import SwaggerParser from '@apidevtools/swagger-parser';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import { readFile } from 'node:fs/promises';
import assert from 'node:assert/strict';

const root = new URL('../../', import.meta.url);
export const contract: any = await SwaggerParser.validate(new URL('contracts/openapi/customer-fabric.yaml', root).pathname, {resolve:{http:false}});
assert.equal(contract.openapi, '3.1.0');
export const ajv = new Ajv2020({allErrors:true, strict:false, validateFormats:true});
addFormats(ajv);
for (const schema of Object.values(contract.components.schemas)) ajv.compile(schema as object);
for (const event of ['created','updated']) {
  const schema = JSON.parse(await readFile(new URL(`contracts/events/profile.${event}.v1.schema.json`, root),'utf8'));
  ajv.compile(schema);
}
console.log('PASS: OpenAPI 3.1 document, resolved references, component schemas and outbox schemas');
