#!/usr/bin/env bash
# Demonstrates source event ingestion with curl against the running M1 stack. Synthetic data only.
set -euo pipefail
cd "$(dirname "$0")/.."
api="http://$1"
set -a; . ./.env; . ./.env.m1; set +a
token() { python3 -c 'import json,os,sys; print(json.loads(os.environ["CF_DEV_TOKENS"])[sys.argv[1]])' "$1"; }
importer=$(token importer)
console=$(token console)
operator=$CF_DEV_OPERATOR_TOKEN
run=$(python3 -c 'import uuid; print(uuid.uuid4().hex)')
record="demo-$run"
out=$(mktemp); trap 'rm -f "$out"' EXIT

if_match=""

# step LABEL EXPECTED_STATUS EXPECTED_TEXT TOKEN METHOD PATH [BODY]
step() {
  local label=$1 expected=$2 text=$3 bearer=$4 method=$5 path=$6 body=${7:-}
  local args=(-sS -o "$out" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $bearer")
  if [ -n "$if_match" ]; then args+=(-H "If-Match: \"$if_match\""); fi
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -H "Idempotency-Key: demo-key-$run-$RANDOM" -d "$body")
  local status; status=$(curl "${args[@]}" "$api$path")
  printf '%-44s HTTP %s  %s\n' "$label" "$status" "$(head -c 400 "$out")"
  [ "$status" = "$expected" ] && grep -q "$text" "$out" || { echo "FAIL: expected HTTP $expected containing $text"; exit 1; }
}
field() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$out" "$1"; }
event() { # ID EPOCH SEQUENCE CHANGE
  printf '{"event_id":"%s-%s","type":"record.updated.v1","record":{"namespace":"crm","value":"%s"},"source_epoch":%s,"sequence":%s,"occurred_at":"2026-01-01T00:00:00Z",%s}' \
    "$run" "$1" "$record" "$2" "$3" "$4"
}

step "Operator reads the source epoch" 200 '"epoch"' "$operator" GET /v1/operator/sources/importer/epoch
old=$(field epoch)
if_match=$old
step "Operator advances it" 200 '"epoch"' "$operator" POST /v1/operator/sources/importer/epoch
if_match=""
epoch=$(field epoch)
step "Operator reads the other source's epoch" 200 '"epoch"' "$operator" GET /v1/operator/sources/console/epoch
console_epoch=$(field epoch)

step "Source creates its record" 201 '"id"' "$importer" POST /v1/profiles \
  "{\"set\":{\"display_name\":\"Synthetic customer\"},\"identifiers\":[{\"namespace\":\"crm\",\"value\":\"$record\"}]}"
profile=$(field id)

first=$(event a "$epoch" 10 '"set":{"tier":"gold"}')
step "Event 10 is applied" 201 '"outcome":"applied"' "$importer" POST /v1/source-events "$first"
step "Retrying it returns the original receipt" 200 '"outcome":"applied"' "$importer" POST /v1/source-events "$first"
step "Reusing its ID with other content is refused" 409 'Event ID conflict' "$importer" POST /v1/source-events \
  "$(event a "$epoch" 10 '"set":{"tier":"platinum"}')"
step "Late event 9 is stale and changes nothing" 201 '"outcome":"stale"' "$importer" POST /v1/source-events \
  "$(event b "$epoch" 9 '"set":{"tier":"bronze"}')"
step "Event 11 is applied" 201 '"outcome":"applied"' "$importer" POST /v1/source-events \
  "$(event c "$epoch" 11 '"set":{"region":"eu"}')"
step "Another source cannot reach this record" 404 'Unknown record' "$console" POST /v1/source-events \
  "$(event d "$console_epoch" 12 '"set":{"tier":"forged"}')"
step "A forged credential is refused" 401 'Unauthorized' "not-a-real-credential" POST /v1/source-events \
  "$(event e "$epoch" 13 '"set":{"tier":"forged"}')"
step "An old source epoch is refused" 409 'Source epoch mismatch' "$importer" POST /v1/source-events \
  "$(event f "$old" 14 '"set":{"tier":"replayed"}')"
step "Profile has event values and provenance" 200 '"tier":"gold"' "$operator" GET "/v1/operator/profiles/$profile"
grep -q '"region":"eu"' "$out" && grep -q '"rule":"source-record"' "$out" && ! grep -q 'bronze\|platinum\|forged\|replayed' "$out" \
  || { echo "FAIL: profile did not match the applied events"; exit 1; }
echo "PASS: source event ingestion for profile $profile"

# Typed attributes: definitions are immutable and the database is reused, so define each one once.
define() { # NAME BODY
  local status; status=$(curl -sS -o "$out" -w '%{http_code}' -H "Authorization: Bearer $operator" "$api/v1/operator/attributes/$1")
  if [ "$status" = 404 ]; then
    step "Operator defines $1" 201 '"revision":1' "$operator" POST "/v1/operator/attributes/$1" "$2"
  else
    step "Operator reads existing $1" 200 '"revision"' "$operator" GET "/v1/operator/attributes/$1"
  fi
}
define loyalty_tier '{"type":"enum","values":["bronze","silver","gold"]}'
define lifetime_value '{"type":"decimal"}'
define signup_date '{"type":"date"}'

step "Source creates a second record" 201 '"id"' "$importer" POST /v1/profiles \
  "{\"set\":{\"display_name\":\"Second synthetic customer\"},\"identifiers\":[{\"namespace\":\"crm\",\"value\":\"$record-2\"}]}"
second=$(field id)
typed=$(event t1 "$epoch" 20 '"set":{"loyalty_tier":"silver","lifetime_value":120.5,"signup_date":"2016-03-01"}')
step "Typed event on record 1 is applied" 201 '"outcome":"applied"' "$importer" POST /v1/source-events "$typed"
record_one=$record; record="$record_one-2"
step "Typed event on record 2 is applied" 201 '"outcome":"applied"' "$importer" POST /v1/source-events \
  "$(event t2 "$epoch" 1 '"set":{"loyalty_tier":"bronze","lifetime_value":40}')"
record=$record_one
step "Valid update on record 1 is applied" 201 '"outcome":"applied"' "$importer" POST /v1/source-events \
  "$(event t3 "$epoch" 21 '"set":{"loyalty_tier":"gold"}')"
step "Retrying the typed event returns its receipt" 200 '"outcome":"applied"' "$importer" POST /v1/source-events "$typed"
step "Late typed event is stale" 201 '"outcome":"stale"' "$importer" POST /v1/source-events \
  "$(event t4 "$epoch" 19 '"set":{"loyalty_tier":"bronze"}')"
step "An undefined enum value is refused" 422 'Invalid attribute value' "$importer" POST /v1/source-events \
  "$(event t5 "$epoch" 22 '"set":{"loyalty_tier":"platinum"}')"
step "An impossible date is refused" 422 'Invalid attribute value' "$importer" POST /v1/source-events \
  "$(event t6 "$epoch" 22 '"set":{"signup_date":"2016-02-30"}')"
step "Record 1 keeps its governed values" 200 '"loyalty_tier":"gold"' "$operator" GET "/v1/operator/profiles/$profile"
grep -q '"attribute_revision":1' "$out" && grep -q '"signup_date":"2016-03-01"' "$out" && ! grep -q 'platinum\|2016-02-30' "$out" \
  || { echo "FAIL: record 1 did not keep its governed values"; exit 1; }
step "Record 2 has its own values" 200 '"loyalty_tier":"bronze"' "$operator" GET "/v1/operator/profiles/$second"
echo "PASS: typed attributes for profiles $profile and $second"
