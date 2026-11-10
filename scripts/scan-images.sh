#!/usr/bin/env bash
set -euo pipefail
# Image archives avoid giving the scanner privileged Docker socket access.
mkdir -p .tools/security
scanner='aquasec/trivy:0.76.1@sha256:790fd66d1697395d8693d1acaa1ea702406a20f109b6fc7bac8db623eca6c06c'
project=$(python3 scripts/compose-project.py)
images=(
  "$project-api:latest"
  "$project-admin-production:latest"
  "$project-db:latest"
  'oven/bun:1.4.4-alpine@sha256:4cf82f5686b196504fe119db3fcd9185b191f82ece82c40680e597228ec6070a'
)
names=(api admin postgres bun)
identity_report=0
if [[ "${1:-}" == --identity-report ]]; then
  identity_report=1
  images=(
    'oryd/kratos:v26.2.0-distroless@sha256:481bfc3022e5427ffb94570eef84480b99c9f8158388378c57df8c1a4a104b3d'
    'axllent/mailpit:v1.33.0@sha256:9b3a463be15354c0f0eba91697d80e3e0d8a5623ef0e627cacc836bc17fd5231'
    'oryd/hydra:v26.2.0-distroless@sha256:ad53a123ddf869fc23ea74f3d76b47e2966dc52f559e93ab31f81440f4d60c5e'
  )
  names=(kratos mailpit hydra)
elif [[ "${1:-}" == --reference ]]; then
  images=("$project-customer-reference:latest")
  names=(customer-reference)
fi
failed=0
for i in "${!images[@]}"; do
  image="${images[$i]}"
  name="${names[$i]}"
  if [[ "$image" == "$project"-* ]]; then
    docker image inspect "$image" >/dev/null
  else
    docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image"
  fi
  if [[ "$identity_report" == 1 ]]; then
    docker run --rm -v "$PWD/.tools/security:/reports" -v customer-fabric-trivy-cache:/root/.cache/trivy       "$scanner" image --image-src remote --scanners vuln --format json --output "/reports/ci-$name.json" "$image"
  else
  docker image save -o ".tools/security/ci-$name.tar" "$image"
  docker run --rm -v "$PWD/.tools/security:/reports" -v customer-fabric-trivy-cache:/root/.cache/trivy \
    "$scanner" image --input "/reports/ci-$name.tar" --scanners vuln \
    --format json --output "/reports/ci-$name.json" --exit-code 0
  fi
  # Preserve all severities in reports; fail on high/critical, including unfixed findings.
  result=0
  python3 - "$name" <<'PY' || result=$?
import json, sys, collections
name=sys.argv[1]
data=json.load(open('.tools/security/ci-'+name+'.json'))
v=[v for r in data.get('Results',[]) for v in r.get('Vulnerabilities',[])]
print(name, dict(collections.Counter(x['Severity'] for x in v)))
sys.exit(42 if any(x['Severity'] in ('HIGH','CRITICAL') for x in v) else 0)
PY
  case "$result" in
    0) ;;
    42) failed=1 ;;
    *) exit "$result" ;;
  esac
done
if [[ "$identity_report" == 1 ]]; then
  echo "Identity image report is for synthetic development. Open findings require assessment before production."
  exit 0
fi
exit "$failed"
