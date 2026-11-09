---
name: feature-delivery
description: Implement a focused Customer Fabric feature or fix, verify it, and prepare reviewable commits and a pull request.
---

1. Inspect `git status --short --branch`, upstream, worktrees, and open PRs for the head and intended base. Keep unrelated edits intact; use a fresh worktree when necessary.
2. Read the relevant ADRs. For endpoint changes, include OpenAPI and tests in the same change. Use [the local development guide](../../../docs/development/local.md) to choose verification and run `make test` with real PostgreSQL.
3. Review staged paths and hunks; commit one meaningful change at a time using `git commit -s`. Keep generated output with its inputs. Run independent review against the intended PR diff before proposing it, and address actionable findings.
4. Prepare a title and description explaining the behavior, verification actually run, and any skipped checks or migration impact. Publish a draft only when requested; use GitHub Actions results for the actual head before marking ready. Leave merging to the maintainer.
