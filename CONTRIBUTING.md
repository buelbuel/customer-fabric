# Contributing

Start with README, the architecture ADR and AGENTS.md. Keep pull requests focused, document API changes, and include meaningful tests for invariants. Run `make test` before submitting and disclose any skipped checks. Use only synthetic customer data in tests and issues.

Contributions are licensed under Apache-2.0. No separate CLA is required.

- **Human-authored commits:** sign off with `git commit -s` to certify the Developer Certificate of Origin 1.1 (https://developercertificate.org/). Use your real identity, and never sign off on another person's behalf.
- **AI-generated commits** (for example by the maintainer's Claude Code agent): attribute them transparently with a `Co-Authored-By` trailer naming the model. They carry no `Signed-off-by` and are **not** DCO-certified; an agent cannot make that certification and nobody signs it on a human's behalf. They are reviewed and merged under the same pull request, independent review and required CI rules as any other change.

Discuss new infrastructure or major domain behavior in an ADR before introducing it. Report security concerns privately as described in SECURITY.md.
