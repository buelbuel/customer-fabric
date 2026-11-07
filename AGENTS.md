# Working on Customer Fabric

- Preserve the Go modular monolith and single backend module. Keep domain rules independent of HTTP and database drivers. Read relevant ADRs before architectural changes; read README and ADR 0001 before changing mutation semantics. Record major decisions in ADRs. Avoid speculative domains, infrastructure, SDKs and visual builders.
- Use parameterized SQL, pgx/v5, pgxpool, sqlc and goose. Keep profile, provenance and outbox writes atomic in a native pgx transaction. Never hand-edit generated queries or add compatibility for unreleased schemas.
- Treat authentication, authorization and data provenance separately. Development auth must fail closed in other modes. Use synthetic data only; never log tokens, payloads or customer values.
- Never overwrite local environment files, copy real secrets to worktrees, or silently delete databases or volumes.
- Update OpenAPI and tests with endpoint changes. Verify changes before committing and disclose skipped checks. GitHub Actions is the verification gate.
- Preserve existing work and keep commits small and focused. Apache-2.0 contributions require DCO sign-off. Obtain independent review before proposing a PR; never merge into main automatically.

Use the applicable procedures in [.agents/skills](.agents/skills) and the [developer workflow](docs/development/workflow.md).
