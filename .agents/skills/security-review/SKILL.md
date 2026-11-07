---
name: security-review
description: Review Customer Fabric code, configuration, dependencies, or images for security regressions.
---

Read [SECURITY.md](../../../SECURITY.md), [ADR 0004](../../../docs/adr/0004-authentication-boundaries.md) and relevant lifecycle or architecture ADRs. Trace changed inputs through authentication, authorization, provenance, persistence and output as separate boundaries. Look for credential or customer data disclosure, development mode bypass, cross-source writes, SQL injection, and broken transaction or rollback behavior. Use synthetic inputs for negative tests.

Use `make security` for Go reachability and Bun dependency checks as applicable. GitHub Actions also scans built images and uploads reports. For findings, show the changed path, exploit preconditions, impact and verification evidence. Distinguish package presence from reachability and disclose checks that did not run. Follow the private reporting route in SECURITY.md.
