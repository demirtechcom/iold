# Architecture Decision Log

## Accepted

### D-001 — Scope is an open-source RunPod bootstrap CLI

IOLD starts supported models on a GPU Pod the user has already rented and registers them with a central gateway. Provisioning, Kubernetes, training, multi-cloud, and general MLOps are excluded.

### D-002 — Go CLI with a single release binary

Core behavior ships as a versioned Go binary. A small install script selects the platform build, verifies its checksum/signature, and installs it. This gives a low-friction installation without placing lifecycle logic in shell scripts.

### D-003 — Linux amd64 and NVIDIA first

The first support target is RunPod Linux/Ubuntu-compatible NVIDIA GPU Pods on amd64. Actual compatibility is defined by the pinned container image and tested GPU matrix, not by claiming support for every Ubuntu/NVIDIA combination.

### D-004 — Do not run nested Docker on RunPod Pods

RunPod already runs the Pod as a container and documents that Docker Compose/starting another Docker instance is unsupported. The supported deployment artifact will be a pinned image derived from the official vLLM image, containing IOLD.

### D-005 — Catalog-first models (superseded by D-017)

Only tested, revision-pinned open models are supported initially. Private and gated models are excluded from v0.

Superseded 2026-07-30: see D-017. The catalog remains as a verified fast path, but any Hugging Face model may be deployed after the planner validates hardware fit.

### D-006 — Minimal lifecycle commands

The initial lifecycle surface is `deploy`, `add`, `status`, `logs`, and `destroy`, with `doctor` as a diagnostic command. `deploy` replaces the primary deployment. `add` creates another vLLM process only after conservative capacity checks.

### D-007 — No reboot persistence

IOLD does not automatically restart services after the RunPod Pod restarts in v0. It does reconcile state whenever invoked.

### D-008 — Full deployment cleanup

Destroy unregisters the gateway entry and deletes all deployment-owned runtime data, cache, logs, and secrets. It does not terminate or stop billing for the RunPod Pod.

### D-009 — Registration failure does not kill healthy inference

A healthy endpoint remains running if gateway registration fails. Registration retries with bounded exponential backoff and the split state remains visible.

### D-010 — Apache License 2.0 and English documentation

The repository uses Apache-2.0. Source, CLI help, issues, and documentation are English-first.

### D-011 — Pin versions and revisions

Release manifests pin the IOLD version, vLLM image digest, CUDA-compatible runtime lineage, model revision, and catalog schema version. Floating `latest` tags are forbidden in release configurations.

### D-012 — Unsloth supplies artifacts, not runtime software

IOLD does not install or run Unsloth. The initial catalog selects `unsloth/Qwen3.6-35B-A3B-NVFP4-Fast` from Hugging Face, and vLLM downloads and serves it. The first hardware target is one RTX PRO 6000 Blackwell with 96 GB VRAM.

### D-017 — Any Hugging Face model is deployable; planner gates deployment

Decided 2026-07-30 (product owner). Supersedes the catalog-only restriction in D-005. IOLD resolves any Hugging Face model ID at deploy time. Before any download or start, a planner inspects the host GPU (VRAM, architecture) and the model metadata (parameter count, dtype/quantization, config) to estimate VRAM need. Models that do not fit are rejected with an explanation and quantized-variant suggestions. Estimates are heuristic and deliberately conservative; a model that "fits" can still fail on real hardware, so readiness checks remain mandatory.

### D-018 — Gated models supported via HF_TOKEN

Decided 2026-07-30 (product owner). Gated Hugging Face models (Llama, Gemma, …) are supported from v0. The token is read from the `HF_TOKEN` environment variable, passed to Hugging Face API calls and to vLLM's download step, never accepted as a CLI flag, and always redacted from logs.

### D-019 — RunPod proxy is the v0 exposure mechanism

Decided 2026-07-30 (product owner). Resolves D-015. v0 uses the RunPod HTTP proxy (`https://<pod-id>-<port>.proxy.runpod.net`). Cloudflare Tunnel work is dropped from v0 scope. The documented 100-second proxy connection limit must be measured against streaming chat on the first rented GPU; results decide whether mitigation (keep-alive chunking, client retry guidance) is needed.

### D-020 — Gateway is the user's own service

Confirmed 2026-07-30: the central gateway is a service owned and written by the product owner, consumed by chat.demirtech.com and agents.demirtech.com. The mock OpenAPI contract stays in place until the real repository is inspected (M0-01).

## Proposed, awaiting validation

### D-013 — Working project and CLI name is `iold`

`iold` is the direct acronym for Instant Open-source LLM Deployment. It is functional but not especially pronounceable; naming and repository availability should be checked before public release.

### D-014 — SQLite for local state; PostgreSQL and Vault centrally

SQLite provides transactional local ownership/reconciliation state without adding a network dependency. PostgreSQL belongs to the central gateway. Vault stores secrets, not logs. Confirm after inspecting the gateway repository.

### D-015 — RunPod proxy for v0, Cloudflare Tunnel for production (resolved by D-019)

RunPod proxy minimizes setup but has a documented 100-second HTTP limit and a public origin URL. Cloudflare Tunnel provides outbound-only origin connectivity and stronger policy options. Resolved 2026-07-30: RunPod proxy is the v0 mechanism and Cloudflare Tunnel is out of scope (D-019).

### D-016 — Gateway management API is the integration boundary

Direct mutation of gateway config or PostgreSQL from a RunPod Pod is rejected. If the existing admin panel lacks an API, add an authenticated, idempotent management endpoint to the gateway repository.

## Deferred

- Raw vLLM flags/advanced escape hatch
- Automatic service startup after reboot
- Unknown Hugging Face model deployment
- GPU auto-selection and capacity optimization
- Metrics export and centralized log shipping
- Signed release provenance beyond checksum verification
