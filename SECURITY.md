# Security policy

This is a development-only foundation, not a supported production release. Use synthetic data and keep services bound to loopback. See docs/security/eu-first.md for the threat model and missing controls.

Do not post exploits, credentials or customer data in public issues. Use the repository's private vulnerability reporting feature when enabled. If it is unavailable, request a private contact from a maintainer without sharing sensitive details. A dedicated reporting address, response SLA and supported-release policy must be established before a public production release. No response SLA is claimed today.

When reporting, include affected revision, a minimal synthetic reproduction, impact and proposed mitigation. Rotate exposed credentials immediately. Dependency and container updates require tests and vulnerability review.

## Build and supply-chain policy

CI pins every GitHub Action to a full commit SHA (the version is kept in a comment) and runs with read-only repository permissions; checkouts do not persist the job token. Dependabot proposes weekly grouped updates for Actions, Go modules, Bun and Docker images, and each update passes the same required checks and review as any other change. CI keeps its dependency and image vulnerability gates (`govulncheck`, `bun audit`, Trivy) and publishes a CycloneDX SBOM of the backend module with every run. CI artifacts are kept for 14 days. Release signing and provenance attestations remain open in #19 and #58.
