#!/usr/bin/env bash
set -euo pipefail
# Image archives avoid giving the scanner privileged Docker socket access.
mkdir -p .tools/security
scanner='aquasec/trivy:0.76.1@sha256:790fd66d1697395d8693d1acaa1ea702406a20f109b6fc7bac8db623eca6c06c'
images=(
  'customer-fabric-api:latest'
  'customer-fabric-admin-production:latest'
  'customer-fabric-db:latest'
  'oven/bun:1.4.4-alpine@sha256:4cf82f5686b196504fe119db3fcd9185b191f82ece82c40680e597228ec6070a'
)
names=(api admin postgres bun)
failed=0
for i in "${!images[@]}"; do
  image="${images[$i]}"
  name="${names[$i]}"
  if [[ "$image" == customer-fabric-* ]]; then
    docker image inspect "$image" >/dev/null
  else
    docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image"
  fi
  docker image save -o ".tools/security/ci-$name.tar" "$image"
  docker run --rm -v "$PWD/.tools/security:/reports" -v customer-fabric-trivy-cache:/root/.cache/trivy \
    "$scanner" image --input "/reports/ci-$name.tar" --scanners vuln \
    --format json --output "/reports/ci-$name.json" --exit-code 0
  # Preserve all severities in reports; fail on high/critical, including unfixed findings.
  python3 - "$name" <<'PY' || failed=1
import json, sys, collections
name=sys.argv[1]
data=json.load(open('.tools/security/ci-'+name+'.json'))
v=[v for r in data.get('Results',[]) for v in r.get('Vulnerabilities',[])]
print(name, dict(collections.Counter(x['Severity'] for x in v)))
sys.exit(any(x['Severity'] in ('HIGH','CRITICAL') for x in v))
PY
done
exit "$failed"
