# Task Backlog

Statuses: `BLOCKED`, `READY`, `IN PROGRESS`, `DONE`.

## Milestone 0 — Required inputs

| ID | Status | Task | Dependency / evidence |
|---|---|---|---|
| M0-01 | IN PROGRESS | Receive and inspect gateway repository | 2026-07-30: gateway identified as Open WebUI + Ollama at `https://llm.demirtech.com` (Ollama-style `/api/generate`, `sk-openwebui-*` keys, base URLs without `/v1`); agents run on kagent (`kagent.dev/v1alpha2` CRDs). Admin-API inspection pending |
| M0-02 | IN PROGRESS | Inspect gateway OpenAPI and admin/config registration flow | Registration likely = add OpenAI-compatible connection in Open WebUI + kagent ModelConfig; verify Open WebUI admin API endpoints |
| M0-03 | BLOCKED | Define register, inspect, refresh, and unregister contract | M0-02 |
| M0-04 | IN PROGRESS | Receive gateway staging URL and auth variable names | Prod URL known; a fresh admin API key is needed in `.env` (one key was exposed in chat on 2026-07-30 and must be rotated), values never committed |
| M0-05 | IN PROGRESS | Pin `unsloth/Qwen3.6-35B-A3B-NVFP4-Fast` immutable revision | Artifact selected; commit pin pending GPU validation |
| M0-06 | IN PROGRESS | Validate RTX PRO 6000 Blackwell 96 GB and storage layout | Hardware selected; RunPod test pending |
| M0-07 | DONE | Validate `iold` name and repository availability | 2026-07-30: pkg.go.dev/npm/PyPI/crates/brew all free; only archived 2018 Rust lib `antoyo/iold`; no trademark/product conflicts. Action: confirm ownership of the existing `github.com/demirtech` account before publishing |
| M0-08 | DONE | Choose proxy or tunnel based on streaming/security test | 2026-07-30 decided D-019: RunPod proxy for v0; 100 s streaming limit still to be measured on first rented GPU |
| M0-09 | IN PROGRESS | User rents first RunPod GPU (cheaper card, e.g. 4090/A40) for streaming and lifecycle validation | Replaces RTX PRO 6000 as first test target; M0-06 evidence updated |

## Milestone 1 — Repository foundation

| ID | Status | Task |
|---|---|---|
| M1-01 | DONE | Initialize Go module and CLI skeleton |
| M1-02 | DONE | Add Apache-2.0, README, contributing, security, conduct, and ownership files |
| M1-03 | DONE | Add `.gitignore` and placeholder-only `.env.example` |
| M1-04 | DONE | Configure GitHub Actions for format, lint, test, race, static analysis, vulnerability and secret scans |
| M1-05 | DONE | Configure versioned Linux amd64 releases with checksums |
| M1-06 | DONE | Implement idempotent checksum-verifying install script |

## Milestone 2 — RunPod runtime

| ID | Status | Task |
|---|---|---|
| M2-01 | BLOCKED | Pin official vLLM base image by digest | M0-05, M0-06 |
| M2-02 | BLOCKED | Build IOLD-enabled RunPod image | M2-01 |
| M2-03 | BLOCKED | Define RunPod template ports, volume, disk, and environment | M0-06, M0-08 |
| M2-04 | DONE | Implement `doctor` checks with injectable system probes |
| M2-05 | BLOCKED | Validate template on first real GPU | M2-02, M2-03 |

## Milestone 3 — Model lifecycle

| ID | Status | Task |
|---|---|---|
| M3-01 | BLOCKED | Define catalog schema and first model entry (verified fast path per D-017) | M0-05 |
| M3-10 | DONE | Implement Hugging Face model resolver (metadata, config, gated/HF_TOKEN, search) per D-017/D-018 |
| M3-11 | DONE | Implement hardware-fit planner: VRAM estimate, fit verdict, quantized-variant suggestions (`iold plan`) per D-017 | Validated live against huggingface.co incl. packed-AWQ sizing and MLX/GGUF filtering |
| M3-12 | DONE | Wire planner into `deploy` as the pre-download gate | HF deploys are rejected before any side effect when the planner says TOO_BIG |
| M3-02 | DONE | Implement transactional local state and migrations |
| M3-03 | DONE | Implement process supervisor, log capture, PID/port ownership, and reconciliation |
| M3-04 | DONE | Implement `deploy` with replace semantics | v0 replace = destroy-old-then-start (downtime, requires `--replace`); transactional keep-old-until-ready deferred. Accepts catalog aliases and any HF ID via the planner gate (D-017). Real-GPU validation pending (M0-09) |
| M3-05 | DONE | Implement `status` and JSON output |
| M3-06 | DONE | Implement redacted `logs` |
| M3-07 | DONE | Implement ownership-safe `destroy` and `--all` |
| M3-08 | DONE | Implement health, model-list, and inference readiness probes | `/health` poll + `/v1/models` + deterministic chat completion before READY; per-deployment API key generated and injected via `VLLM_API_KEY` env |
| M3-09 | DONE | Implement retry, timeouts, cancellation, and stable error codes |

## Milestone 4 — Exposure and gateway

| ID | Status | Task |
|---|---|---|
| M4-01 | DONE | Implement RunPod proxy URL resolution | `RUNPOD_POD_ID` env -> `https://<pod>-<port>.proxy.runpod.net`, local URL otherwise |
| M4-02 | BLOCKED | Measure streaming behavior and 100-second proxy constraint | M2-05 |
| M4-03 | DONE | Implement named Cloudflare Tunnel mode if selected | Dropped from v0 scope per D-019; no implementation needed |
| M4-04 | BLOCKED | Implement gateway client and secret injection | M0-03, M0-04 |
| M4-05 | BLOCKED | Implement independent registration reconciliation | M4-04 |
| M4-06 | BLOCKED | Implement unregister-before-destroy flow | M4-04 |

## Milestone 5 — Verification

| ID | Status | Task |
|---|---|---|
| M5-01 | DONE | Build fake vLLM and fake gateway test fixtures |
| M5-02 | DONE | Implement failure-path and crash-recovery component suite |
| M5-03 | BLOCKED | Create protected RunPod integration-test workflow with guaranteed external teardown | M0-06 |
| M5-04 | BLOCKED | Run Chat staging discovery and inference test | M4-05 |
| M5-05 | BLOCKED | Run Agents staging discovery and inference test | M4-05 |
| M5-06 | BLOCKED | Verify complete cleanup and residual gateway detection | M4-06 |
| M5-07 | BLOCKED | Record median/p95 lifecycle timing | M5-03 through M5-06 |

## Milestone 6 — Multiple models

| ID | Status | Task |
|---|---|---|
| M6-01 | BLOCKED | Define validated colocation matrix and conservative resource accounting | Single-model release evidence |
| M6-02 | BLOCKED | Implement `add`, deployment selection, and port allocation | M6-01 |
| M6-03 | BLOCKED | Isolate keys, logs, tunnels, state, and gateway records per deployment | M6-02 |
| M6-04 | BLOCKED | Test independent lifecycle and wrong-target protection | M6-03 |

## Definition of done

A task is done only when implementation, automated tests, relevant failure paths, user-facing documentation, redaction/security review, and reproducible verification evidence are complete. A milestone is not complete while it can leave an IOLD-owned process, endpoint, secret, file, or gateway registration without reporting it.
