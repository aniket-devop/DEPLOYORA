# Architecture Decision Records

Status of every record below: **Proposed**. A record becomes **Accepted** only after the founder approves the matching question in `docs/roadmap.md` Appendix A. Each record states what would make us revisit it.

Format: Context, Decision, Reasons, Alternatives considered, Consequences, Revisit when.

---

## ADR-001: Modular monolith in a single Go module

**Context:** one maintainer (initially), a CLI and a server that must share logic, and a need for contributors to understand the code quickly.

**Decision:** one repository, one `go.mod`, packages under `internal/` with enforced dependency rules. Two binaries (`deployora`, `deployora-server`) built from the same module.

**Reasons:** lowest operational and cognitive cost; refactoring across boundaries is cheap; the CLI works with no network service at all.

**Alternatives:** microservices (needless distributed-systems cost); multiple Go modules (versioning overhead without a second consumer); a Kubernetes operator with CRDs (couples the product to cluster installation, and conflicts with local-first CLI use).

**Consequences:** boundaries are enforced by an import linter, not by process isolation. We must keep discipline in code review.

**Revisit when:** a component needs independent scaling or a separate release cadence (likely the worker, not before v1.0).

---

## ADR-002: One engine, two front doors, via an application layer

**Context:** requirement that a single core powers the CLI and REST API.

**Decision:** `internal/app` exposes use cases (`RunTest`, `Analyze`, `ListRuns`, `ApproveProposal`). `internal/cli` and `internal/server` are thin adapters that only parse input, call `app`, and format output.

**Reasons:** prevents behavior drifting between CLI and API; tests target the use cases once.

**Alternatives:** CLI calls the API even locally (forces a server for a laptop run, hurting adoption); API shells out to the CLI (fragile, poor error handling).

**Consequences:** `app` must stay free of HTTP and Cobra types.

**Revisit when:** a third front door appears (for example a GitHub App); the same pattern should hold.

---

## ADR-003: Kind is the only cluster provider in the MVP, with a hard production guard

**Context:** the requirements say isolated temporary clusters and never target production in the MVP.

**Decision:** a `ClusterProvider` interface exists, but only the Kind implementation ships. The engine never reads `~/.kube/config` or `KUBECONFIG`. It writes a private kubeconfig into the run directory, passes it explicitly to every subprocess and client, scrubs the environment of child processes, and refuses to operate on clusters not named `deployora-<runid>`. The CLI exposes no `--context` or `--kubeconfig` flag.

**Reasons:** the safest way to never hit production is to make it impossible to point the tool at anything it did not create. A warning or a confirmation prompt is not a control.

**Alternatives:** allow `--context` with a denylist or prompt (denylists fail open); support k3d/minikube immediately (more surface, no new value for the MVP).

**Consequences:** users who want to test in an existing cluster must wait. The interface keeps that door open later behind an explicit allowlist.

**Revisit when:** there is real demand for bring-your-own-cluster, and only with namespaced read-only roles and allowlists.

---

## ADR-004: Orchestrate existing tools; do not build an assertion DSL

**Context:** Chainsaw and KUTTL already provide declarative assertions, and Chainsaw has strong momentum and CNCF-adjacent backing (see competitor analysis).

**Decision:** built-in checks stay small and opinionated (readiness, endpoints, Jobs, HTTP smoke). Anything richer is handled by runner adapters (Chainsaw first) that feed results into the same report and history. No custom expression or assertion language.

**Reasons:** a DSL is a permanent maintenance and support burden, and we would lose to incumbents on expressiveness. Being a complement lowers the adoption barrier.

**Alternatives:** build a YAML assertion format (rejected); ignore existing tools entirely (loses users who already have suites).

**Consequences:** the `Check` interface must be general enough for an adapter that runs an external process and parses its report.

**Revisit when:** user research shows the built-in checks cannot cover the top use cases and adapters are insufficient.

---

## ADR-005: Deterministic core; AI is optional and isolated

**Context:** core testing must work when the Claude API is unavailable, and AI must not be able to affect pass/fail.

**Decision:** the `ai` package depends only on `domain`, `evidence` and `redact`. The engine never imports it. Analysis is a separate use case run after a run completes, reading the evidence bundle. Analysis failure never changes run status or exit code.

**Reasons:** a compile-time guarantee beats a convention. It also makes the core testable without network or cost.

**Alternatives:** AI as a pipeline stage (couples availability and cost to every run); AI embedded in collectors (leaks cluster access to the AI path).

**Consequences:** the AI module cannot request more data from the cluster. Evidence selection must be good enough up front.

**Revisit when:** benchmarks show iterative evidence-gathering greatly improves accuracy. If so, add it as a bounded, read-only, human-visible step, still outside the engine.

---

## ADR-006: Evidence bundle as the central contract

**Context:** reports, history, dashboard and AI all need the same facts about a run.

**Decision:** every run writes a versioned on-disk bundle with stable evidence IDs (`ev:event:12`, `ev:log:<pod>:<container>`). Downstream components read the bundle, not the cluster.

**Reasons:** reproducibility, shareable bug reports, offline analysis, cheap tests with fixture bundles, and verifiable AI citations.

**Alternatives:** keep data only in the database (not portable, no CLI-only mode); re-query the cluster for analysis (the cluster is gone after teardown).

**Consequences:** the bundle schema is a public contract and must be versioned with compatibility tests.

**Revisit when:** bundle size becomes a problem; add compression and retention policies.

---

## ADR-007: Storage behind an interface: files locally, PostgreSQL for the server

**Context:** the stack requires PostgreSQL for persistent history, but a CLI-only user should not need a database.

**Decision:** a `Store` interface with two implementations. Local mode uses JSON files plus an index. Server mode uses PostgreSQL (pgx, SQL migrations, typed queries via sqlc or equivalent). The same contract test suite runs against both.

**Reasons:** zero-dependency local adoption; Postgres where concurrency, queueing and multi-user access matter.

**Alternatives:** Postgres for everything (kills the local-first story); SQLite locally and Postgres on the server (two SQL dialects and migration paths for little gain).

**Consequences:** two implementations to maintain, mitigated by contract tests and a deliberately small interface.

**Revisit when:** local history queries outgrow a file index, in which case SQLite becomes worth considering.

---

## ADR-008: Integration approach: Kind as a library, Helm as a CLI, kubeconform as library-or-binary

**Context:** we depend on three external tools with different API stability.

**Decision:** use Kind's Go library for provisioning. Run Helm through its CLI behind the `Deployer` interface instead of embedding the Helm SDK. Use kubeconform as a library if its package API proves stable, otherwise as a pinned binary. `deployora doctor` checks tool versions.

**Reasons:** the Helm SDK changes between releases and the CLI matches what users run. Shelling out costs little here. Kind's library is the supported way to drive it.

**Alternatives:** embed everything (dependency churn); shell out to everything (harder error handling for Kind).

**Consequences:** runtime dependency on a Helm binary and on Docker; documented prerequisites and clear `doctor` output.

**Revisit when:** Helm SDK stability improves or binary distribution becomes a support problem (consider bundling pinned binaries).

---

## ADR-009: Allowlist-first collection and layered redaction

**Context:** Secrets must never reach Claude, and logs may contain anything.

**Decision:** defense in depth, in this order: (1) collectors read only an allowlist of kinds and never `Secret`; (2) structural scrubbing (drop Secret documents from rendered manifests, mask env values, strip managed fields and annotations that commonly hold credentials, never include Helm values); (3) pattern redaction on all text; (4) redaction again at the AI boundary; (5) a `--dry-run` preview of the exact payload. Canary-secret tests run in CI.

**Reasons:** pattern-matching alone will miss novel secret formats. Not collecting is the control; redaction is the backstop.

**Alternatives:** redaction only (fails open); sending only metadata (too little for useful diagnosis).

**Consequences:** some useful context (for example env values) is intentionally unavailable to analysis. That is an accepted trade-off.

**Revisit when:** users demand configurable allow rules for specific env vars; make them explicit opt-ins, never defaults.

---

## ADR-010: Remediation is an inert proposal; humans approve; nothing is executed against clusters

**Context:** requirements forbid unrestricted AI execution and require human approval.

**Decision:** AI remediation output becomes `Proposal` records (`pending`). In the MVP, approval only unlocks writing a unified diff or instructions to a file for the user to review and commit. Deployora never applies model output to any cluster, and no model tool-use is enabled. Approvals are audited with actor and time.

**Reasons:** the clusters are throwaway, so the valuable output is a change to the source manifests or chart. That change belongs in version control with human review.

**Alternatives:** auto-apply in the Kind cluster and re-test (interesting later, but complexity and safety review first); open PRs automatically (needs repo credentials and trust).

**Consequences:** the "fix" loop is slower but safe.

**Revisit when:** v1.x, a re-test loop in an ephemeral cluster with strict diff validation could be added, still requiring human approval to leave the sandbox.

---

## ADR-011: Claude integration policy

**Context:** Anthropic Claude API only; structured JSON; keys never in the frontend.

**Decision:** bring-your-own key, read only by the CLI process or the server worker. The model name is configuration. Responses must conform to a JSON Schema and cite evidence IDs that exist in the bundle; invalid claims are dropped or the analysis is rejected after one repair retry. Per-run token and cost caps. Prompts are versioned files. An evaluation harness over failure fixtures gates prompt and model changes. The structured-output mechanism will be chosen when the module is built, after reading the current API documentation.

**Reasons:** verifiable citations turn hallucination from a trust problem into a validation step. Config-driven models avoid code changes at each model release.

**Alternatives:** free-text answers (unverifiable); a provider-neutral abstraction (explicitly out of scope; the `Analyzer` interface exists for fakes and degradation only).

**Consequences:** analysis quality depends on evidence selection and the eval suite, both of which are real work.

**Revisit when:** eval results show systematic failure modes that need different input shaping.

---

## ADR-012: REST API is OpenAPI-first; the server queue lives in PostgreSQL

**Context:** the dashboard and remote CLI must use a stable API; the server needs durable jobs without extra infrastructure.

**Decision:** `api/openapi.yaml` is the source of truth. Go server stubs and the TypeScript client are generated. Jobs are rows claimed with `FOR UPDATE SKIP LOCKED`. Progress is delivered with server-sent events. The server accepts uploaded archives and does not clone repositories in the MVP.

**Reasons:** one source of truth for types; no Redis or message broker to operate; SSE is simple and enough.

**Alternatives:** Redis or NATS queue (extra component); WebSockets (more complexity than needed); the server cloning git (stores credentials, more attack surface).

**Consequences:** Postgres is the single stateful dependency; throughput is bounded by Docker capacity anyway.

**Revisit when:** queue throughput or fairness becomes a bottleneck.

---

## ADR-013: CLI contract: machine-readable output and exit codes

**Context:** CI adoption depends on predictable behavior.

**Decision:** Cobra CLI. `--output json` everywhere. Exit codes: `0` passed, `1` failed, `2` errored (tool or environment problem), `3` usage error. Human output is for terminals only and may change; JSON output follows the versioned schema.

**Reasons:** CI authors need to tell "your deployment is broken" from "our runner is broken".

**Alternatives:** a single non-zero code (loses the distinction).

**Consequences:** exit codes become part of the public contract.

**Revisit when:** never without a major version bump.

---

## ADR-014: Versioned public schemas

**Context:** CI integrations, dashboards and third parties will parse our outputs.

**Decision:** publish JSON Schemas for the report, evidence index, analysis and config under `schemas/`, each with `schema_version`. Additive changes only within a major version, with compatibility tests against stored fixture bundles.

**Reasons:** stability earns integrations.

**Alternatives:** undocumented JSON (breaks users silently).

**Consequences:** extra discipline on every output change.

**Revisit when:** a breaking change is unavoidable; ship a new major schema version alongside the old one.

---

## ADR-015: Licensing and governance

**Context:** the goal is community adoption.

**Decision (proposed):** Apache-2.0 license (matches the surrounding Kubernetes ecosystem). Developer Certificate of Origin sign-off, no CLA initially. `SECURITY.md` with a private disclosure channel from day one. A short `GOVERNANCE.md` that names the maintainer and the path to adding maintainers. Signed releases with SBOMs.

**Reasons:** permissive and familiar licensing reduces adoption friction; early security hygiene signals seriousness.

**Alternatives:** MIT (fine, but lacks the explicit patent grant); AGPL or source-available (limits adoption; only worth it if the plan is a hosted commercial offering).

**Consequences:** the license is hard to change later. Decide deliberately.

**Revisit when:** commercial strategy is defined. This is a founder question, not a technical one.

---

## ADR-016: Do not claim novelty the competitors already hold

**Context:** every one of the 14 planned features exists in other tools (see competitor analysis).

**Decision:** public messaging leads with the workflow ("will this deploy healthy on a clean cluster, and if not, why?") and with measured results. README, website and talks must not claim "first" or "only" for any individual feature. AI quality claims require a published benchmark.

**Reasons:** credibility is the scarcest resource for a new open-source project; maintainers of existing tools will notice overclaims.

**Consequences:** marketing is slower and more evidence-driven.

**Revisit when:** a feature genuinely becomes unique and can be shown to be.
