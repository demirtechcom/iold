# Testing Strategy

## Quality objective

The tool controls GPU-backed processes and gateway records. The highest-risk failures are leaked resources, exposed endpoints, incorrect cleanup, and a deployment reported ready when it cannot serve inference.

## Test layers

### Unit tests

- CLI parsing and stable exit codes
- catalog schema validation and revision pinning
- GPU/VRAM/disk compatibility decisions using fixtures
- state-machine transitions and illegal-transition rejection
- redaction of tokens, headers, URLs, and environment values
- retry classification, exponential backoff, jitter, and cancellation
- port allocation and multi-model resource accounting
- destroy ownership checks and path-boundary validation
- gateway request signing, idempotency, and response parsing

All packages run with the Go race detector where applicable.

### Component tests

- fake vLLM HTTP server: delayed health, failed load, malformed model list, streaming, auth failure
- fake gateway: conflict, idempotent replay, timeout, partial success, unavailable, and unregister failure
- SQLite crash/reopen and interrupted-transition recovery
- supervisor behavior for SIGTERM, process-group SIGKILL, orphan workers, stale/reused PID plus start-time identity, and occupied port
- cross-process lifecycle locking and concurrent deploy exclusion
- STARTING-intent adoption and dead-runtime reconciliation to CRASHED
- write-time log redaction, including JSON Authorization headers
- install script platform selection and checksum failure

### Container tests

- build the pinned runtime image
- confirm binary, vLLM, CUDA libraries, writable cache, non-secret defaults, and declared ports
- scan image and Go binary for known vulnerabilities and accidental secrets
- start on a GPU-capable runner when available and verify `/v1/models` plus a minimal inference

### Real RunPod integration tests

Run on an explicitly selected low-cost test GPU with a budget/time cap:

1. start supported template and attach persistent storage;
2. run `iold doctor`;
3. deploy the first pinned model;
4. verify authenticated direct inference;
5. verify gateway registration;
6. stream a response through the selected exposure mechanism;
7. interrupt registration and verify recovery;
8. destroy and assert zero owned processes, ports, cache directories, keys, and gateway records;
9. stop the RunPod Pod using the external test harness.

The harness—not IOLD—must own RunPod creation and guaranteed teardown so a failed test cannot leak billing.

### Staging end-to-end tests

- model appears in Chat model selection
- Chat produces a real response through the gateway
- model appears in Agents model selection
- Agents produces a real response through the gateway
- streaming cancellation releases the request
- gateway outage leaves inference healthy and registration later reconciles
- destroy removes discovery from both applications

Production is not the default automated test environment. Use staging. Production smoke tests require an explicit manual release gate and non-destructive test model namespace.

## Acceptance test

```text
single command
-> vLLM starts
-> deterministic inference succeeds
-> gateway registers endpoint
-> Chat sees and uses model
-> Agents sees and uses model
-> destroy unregisters and removes all IOLD-owned runtime data
```

RunPod Pod billing continues until the user or external harness terminates the Pod; the test verifies that warning.

## Failure-path matrix

| Failure | Expected behavior |
|---|---|
| Unsupported GPU or insufficient VRAM | Fail before download/start; no side effects |
| Disk becomes full during download | Stop, record actionable failure, clean partial owned files |
| vLLM exits during startup | Capture redacted logs, mark failed, no gateway registration |
| Health passes but inference fails | Never mark ready |
| Gateway unavailable | Keep healthy runtime, retry registration, show split state |
| CLI receives interrupt | Persist recoverable state; next invocation reconciles |
| Unregister fails during destroy | Retry and surface residual registration; still safely stop local runtime according to policy |
| Local DB is unavailable/corrupt | Do not guess destructive ownership; enter recovery/inspection mode |
| Duplicate deploy request | Apply defined replace semantics without leaking the prior runtime |
| Wrong destroy target | Refuse operation outside owned deployment paths/processes |

## CI gates

- formatting and lint
- unit/component tests
- race detector
- static analysis and vulnerability scan
- license/header checks
- secret scanning
- reproducible Linux amd64 build
- image build and scan
- documentation/link validation

Real GPU and staging tests run on protected workflows with concurrency locks, explicit budget limits, guaranteed teardown, and masked secrets.

## Version matrix

Do not write mutable versions into this document. A machine-readable release manifest will pin:

- Go toolchain
- IOLD binary version and checksum
- vLLM image digest
- supported NVIDIA driver floor/runtime compatibility
- model repository revision
- RunPod template version
- cloudflared image/version when enabled
