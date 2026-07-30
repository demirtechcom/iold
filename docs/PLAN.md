# Implementation Plan

## Delivery principle

Build one measured vertical slice before broadening model or GPU support:

```text
one pinned model + one tested NVIDIA GPU class + pinned vLLM image
-> authenticated endpoint -> gateway -> Chat and Agents -> reliable destroy
```

## Phase 0 — Close contracts

Deliverables:

- inspect gateway repository, OpenAPI document, configuration model, and admin workflow
- define idempotent register/status/unregister contract
- choose staging environment and service credentials
- select first exact Hugging Face repository and immutable revision
- select first RunPod GPU and persistent-volume layout
- decide RunPod proxy versus named Cloudflare Tunnel from measured tests
- validate `iold` project/repository/CLI name

Exit criteria: no unresolved dependency blocks a real end-to-end test.

## Phase 1 — Repository and release foundation

- initialize Go module and conventional repository layout
- add Apache-2.0 license, contribution guide, code of conduct, security policy, and ownership rules
- add lint, unit tests, race detector, vulnerability/static checks, and reproducible builds
- release Linux amd64 binary with checksums
- create an idempotent install script that verifies checksums
- create `.env.example` using placeholders only
- create `.gitignore` for secrets, state, logs, weights, and caches

Exit criteria: a clean machine can install and verify `iold version`.

## Phase 2 — Supported RunPod runtime image

- derive a version-pinned image from official `vllm/vllm-openai`
- add IOLD and a minimal supervisor/entrypoint
- define persistent `/workspace/.iold` and model-cache paths
- validate NVIDIA driver/runtime compatibility through `doctor`
- document RunPod template port, disk, volume, and environment settings

Exit criteria: the template boots and `iold doctor` passes on the selected GPU.

## Phase 3 — Single-model lifecycle

- implement catalog schema and first pinned model entry
- implement GPU, VRAM, disk, process, and port inspection
- implement transactional local state
- implement `deploy`, `status`, `logs`, and `destroy`
- add retry classification, timeouts, signal handling, and crash reconciliation
- perform health, `/v1/models`, and deterministic inference readiness checks
- generate and inject a per-deployment API key

Exit criteria: repeated deploy/destroy cycles leave no IOLD-owned processes, files, ports, or credentials behind.

## Phase 4 — Exposure and gateway registration

- implement RunPod proxy endpoint resolution
- test long streaming responses against the documented proxy timeout
- implement Cloudflare named-tunnel mode if selected
- implement authenticated gateway client with idempotency keys
- retry registration independently of runtime health
- unregister before runtime teardown

Exit criteria: model appears through the central gateway and removal is reflected there.

## Phase 5 — Application end-to-end validation

- confirm model discovery in Chat
- confirm model discovery and inference in Agents
- test streaming, cancellation, timeouts, and unavailable-model behavior
- test gateway restart and temporary network loss
- record time-to-ready measurements by lifecycle stage

Exit criteria: the agreed end-to-end acceptance flow passes against staging.

## Phase 6 — Multiple models

- add `iold add`
- allocate deterministic deployment IDs and ports
- enforce conservative aggregate VRAM and disk budgets
- isolate logs, state, keys, tunnels, and gateway registrations
- support targeted and `--all` status/log/destroy operations

Exit criteria: supported colocated models operate independently and cleanup cannot affect the wrong deployment.

## Phase 7 — Public open-source release

- publish quickstart with a measured time-to-first-endpoint
- document the exact supported matrix and non-goals
- publish troubleshooting and cost/billing warning prominently
- provide demo recording and architecture diagram
- tag a signed/checksummed release and publish the RunPod template image by digest

## Success metrics

- installation success rate
- median and p95 time to ready
- model download, load, gateway registration, and cleanup durations
- successful deploy/destroy cycle rate
- orphaned IOLD process/file/registration count (target: zero)
- successful Chat and Agents inference rate
- percentage of failures with an actionable error code

## Important product warning

`iold destroy` cannot stop RunPod billing because IOLD does not own Pod provisioning. Every destroy result and README quickstart must tell the user to stop or terminate the RunPod Pod separately when finished.

