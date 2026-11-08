# Developer workflow on macOS

1. Install Apple's command line tools (`xcode-select --install`) for Git and Make. Install Python 3, Colima, Docker CLI, and the Compose v2 plugin. With Homebrew: `brew install python colima docker docker-compose`. If Docker cannot find the plugin, follow `brew info docker-compose` to configure plugin discovery while preserving existing Docker settings. Buildx is optional.
2. Start Colima with `colima start`. Verify `docker context show`, `docker info`, and `docker compose version`; select the intended local context before running a target. Put the repository and linked worktrees under a Colima mount (normally your macOS home directory). A checkout under host `/tmp` may appear empty to containers.
3. Install the ChatGPT desktop app for Codex Desktop, or install the [Codex CLI](https://learn.chatgpt.com/docs/codex/cli). Sign in to the client you use. Install GitHub CLI (`brew install gh`) and authenticate with `gh auth login`, or use GitHub in the browser with authenticated Git for pushes. Go, Bun and sqlc come from the tool containers; host installs are optional. No application API key or MCP server is required.

## Start a task

Check `git status --short --branch`, `git worktree list`, and `gh pr status`. Confirm the base and head; extend an existing PR only when the task belongs to it. Leave unrelated edits in their original checkout.

In Desktop, choose a Worktree chat based on the intended branch. In the CLI, create a linked checkout and launch Codex there:

```sh
git fetch origin
# Set BASE to a ref containing the workflow integration.
git worktree add -b codex/short-task ../customer-fabric-short-task "$BASE"
cd ../customer-fabric-short-task
make init
codex
```

Select the checked-in **Customer Fabric** [local environment](https://learn.chatgpt.com/docs/environments/local-environment) in Desktop. Its setup script runs `make init` in the new worktree. It creates fresh, ignored development credentials with mode 0600 and leaves an existing file alone. Keep the setup script small; run the checks as actions. Never copy `.env`, tokens, or data from another checkout.

## Project actions

Run these from the checkout root in the CLI, or choose the matching action in Desktop:

| Action | Command | What it verifies or starts |
| --- | --- | --- |
| Tests | `make test` | SQL generation, real PostgreSQL Go race tests, admin type check and build |
| Contracts | `make contracts` | Starts the local API and validates live OpenAPI behavior and enterprise fixtures |
| Smoke | `make smoke` | Exercises the running local API with synthetic writes; run Local development or Contracts first |
| Local development | `make up` | Starts PostgreSQL, migrations, API and console, then prints their HTTP addresses |
| M1 local development | `make m1-up` | Starts the isolated customer identity stack |
| M1 smoke | `make m1-smoke` | Verifies real native flows using the checkout ports |
| Customer browser | `make m1-browser` | Runs real Kratos self-service in Chromium |
| Session failure checks | `make m1-session-smoke` | Exercises expiry, issuer outage and one settings race in the current project's synthetic stack |
| Settings race | `make m1-settings-race` | Repeats the concurrent email-change race in parallel and writes aggregate outcomes to `.tools/reports/settings-race.json`. `SETTINGS_RACE_ITERATIONS` and `SETTINGS_RACE_WORKERS` default to 40 and 4. Each race makes about seven API calls against the 600-per-minute development limit |
| Enterprise linking | `make m1-linking-init`, `make m1-up`, `make m1-linking-smoke` | Exercises separately credentialed issuer and customer flows |
| Named operators | `make operator-up`, `make operator-smoke`, `make operator-browser` | Starts the separate issuer and verifies MFA and read access |
| Stop | `make down` | Stops the current checkout's services and preserves named volumes |

For linked worktrees, Make scopes the Compose project and its PostgreSQL volume to the checkout path. Docker assigns available loopback ports for the API and both admin services; run `make ports` with the development stack up to see its addresses. The primary checkout keeps its usual ports and project. PostgreSQL has no host port. Use Make targets from worktrees: raw Compose commands in CI use the primary project defaults. Run `make down` before moving, removing, or archiving a checkout; the project name is path based. A retained database volume requires the original local database password.

## Keep core documents current

Review this table for every PR and change the document in the same PR as the behavior. Dated verification reports, security reviews and accepted ADRs are historical records: add a new one rather than rewriting them.

| When you change | Update |
| --- | --- |
| Routes, credentials or audiences | `contracts/openapi/customer-fabric.yaml`, `docs/architecture/overview.md` trust boundaries, `docs/security/eu-first.md` |
| Tables or retained data categories | `docs/architecture/overview.md` data model; ADR 0003 category policy through a new ADR if the policy changes |
| Compose services, images or identity configuration | `docs/development/customer-identity.md` readiness or `docs/development/operator-identity.md`, and `docs/security/eu-first.md` |
| Milestone scope or sequencing | GitHub issue #8 and `docs/architecture/roadmap.md` |

## Commit and prepare a PR

Run `make test` and the checks relevant to the change. Run `make generate` for SQL edits and include the generated output; use the repository skills for database, security, fixtures and feature delivery. Inspect the staged diff, stage only relevant paths or hunks, and use `git commit -s` with one logical change per commit. Get an independent review against the intended base, such as a separate Codex review or a teammate.

When ready to publish, push the named branch and open a draft against the agreed base. Explain the behavior, checks actually run, skipped checks and any migration impact. GitHub Actions is authoritative: inspect the backend formatting, sqlc, PostgreSQL race tests, fixtures, vet, module verification, vulnerability and build checks; admin type checking, build and audit; the fresh image builds, live contracts, audits and image scans; and the customer and operator identity jobs. Use the run for the PR's current head (for example, `gh pr checks --watch`). Leave merging to the maintainer.
