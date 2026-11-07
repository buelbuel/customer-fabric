import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import {readFile} from 'node:fs/promises';
import assert from 'node:assert/strict';
const root=new URL('../../tests/fixtures/enterprise/',import.meta.url);
const json=async(path:string)=>JSON.parse(await readFile(new URL(path,root),'utf8'));
const ajv=new Ajv2020({strict:false,allErrors:true});addFormats(ajv);
const projection=await json('schemas/source-projections.schema.json');
for(const source of ['salesforce','braze','shopify','emarsys','erp','loyalty','identity']){
 const validate=ajv.compile(projection.$defs[source]);
 assert.ok(validate(await json(`curated/sources/${source}/sample.json`)),`${source}: ${ajv.errorsText(validate.errors)}`);
}
const event=ajv.compile(await json('schemas/event.schema.json'));
for(const order of ['occurrence','delivery']){
 const lines=(await readFile(new URL(`sample/events/${order}.ndjson`,root),'utf8')).trim().split('\n');
 for(const line of lines)assert.ok(event(JSON.parse(line)),ajv.errorsText(event.errors));
}
console.log('PASS: curated source projections and committed event stream schemas');
