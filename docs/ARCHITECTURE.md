# Architecture

## 1. Objective

IOLD turns a manually rented RunPod NVIDIA GPU into a gateway-registered, OpenAI-compatible model endpoint. The user selects a supported catalog model; IOLD validates the machine, starts and supervises vLLM, checks readiness, exposes the endpoint, and reconciles gateway registration.

The CLI does not provision or terminate the RunPod machine itself.

## 2. System boundary

```text
RunPod Pod
  IOLD CLI/supervisor
    |-- hardware and prerequisite detection
    |-- model catalog and runtime configuration
    |-- vLLM process lifecycle
    |-- local state and logs
    |-- endpoint exposure adapter
    `-- gateway registration client
                 |
                 v
        Central LLM Gateway
          |             |
          v             v
  chat.demirtech.com  agents.demirtech.com
```

The two application URLs are consumers, not model endpoint URLs. A model receives its own RunPod proxy URL or Cloudflare Tunnel hostname. Both applications continue using the central gateway.

## 3. RunPod execution model

RunPod Pods are already containers. RunPod explicitly states that Docker Compose is unsupported and that users cannot start their own Docker instance inside a Pod. Therefore nested Docker is not a supported architecture.

### Recommended production path

Publish a pinned RunPod-compatible image derived from the official `vllm/vllm-openai` image. Add the statically linked IOLD binary and a small entrypoint. The user selects this image/template when renting the GPU, connects to the Pod, and runs the CLI.

This preserves the one-command model experience while keeping CUDA, Python, and vLLM versions reproducible.

### Bootstrap path

A release install script downloads and verifies the correct IOLD binary. It must not install CUDA, Docker, or vLLM dynamically. On an incompatible generic Ubuntu image, `iold doctor` exits with actionable guidance to use the supported template.

## 4. CLI surface

Proposed command name: `iold`.

```text
iold deploy <catalog-model>   Create or replace the primary deployment
iold add <catalog-model>      Add another model if capacity and port checks pass
iold status [deployment]      Show runtime, endpoint, registration, and resource state
iold logs [deployment]        Stream or print redacted logs
iold destroy [deployment|--all]  Stop runtime, unregister, and remove owned data
iold doctor                   Validate GPU, driver, disk, network, runtime, and credentials
```

`doctor` is included because it is diagnostic rather than another lifecycle concept. `deploy` is replace semantics as requested. Replacement must be transactional: validate the new deployment before deleting a healthy old one whenever GPU memory permits; otherwise warn about downtime and require explicit confirmation.

## 5. Main components

### Command layer

Parses commands, renders stable human-readable output, and optionally emits JSON for automation. It contains no infrastructure logic.

### Host inspector

Reads NVIDIA device information, VRAM, driver compatibility, disk availability, architecture, listening ports, and outbound connectivity. Initial support is Linux amd64 on NVIDIA RunPod Pods.

### Model catalog

Only catalog entries are deployable by default. Each versioned entry pins:

- Hugging Face repository and revision/commit
- served model name
- minimum and recommended VRAM
- supported GPU count
- vLLM image/version compatibility
- dtype, context limit, port, and reviewed runtime arguments
- health and smoke-test prompt expectations

Unknown models are rejected in v0. An advanced raw-flag escape hatch is deferred until the safe path is stable.

### First catalog entry

The alias `qwen3.6-35b-a3b` resolves to:

```text
Base model: Qwen/Qwen3.6-35B-A3B
Artifact:   unsloth/Qwen3.6-35B-A3B-NVFP4-Fast
GPU:        NVIDIA RTX PRO 6000 Blackwell, 96 GB
Runtime:    vLLM
Format:     NVFP4
```

Unsloth is an artifact publisher here, not an installed service or Python dependency. vLLM downloads and serves the artifact. GGUF and MLX artifacts are excluded from the vLLM-first catalog. Each artifact needs hardware-specific load, inference, quality, and cleanup tests.

### Runtime manager

Starts one vLLM server process per deployment, allocates a unique local port, and records the PID together with the OS process-start identity and exact command. Shutdown verifies all three values, signals the dedicated process group, waits for the entire group to empty, and escalates the group to SIGKILL when necessary. Runtime stdout/stderr passes through a detached redacting proxy before it reaches the on-disk log. System reboot persistence is explicitly disabled for v0.

Because a vLLM server hosts one model at a time, multiple models mean multiple vLLM processes. `iold add` must reject a model when the catalog's conservative VRAM budget, GPU topology, disk, or port checks fail.

### Exposure adapter

Two modes are planned:

1. `runpod-proxy`: simplest v0 path. The Pod must expose the chosen HTTP port, yielding `https://<pod-id>-<port>.proxy.runpod.net`.
2. `cloudflare-tunnel`: recommended controlled-ingress path once hostname and tunnel-token automation are defined.

RunPod's HTTP proxy has a documented 100-second connection limit. This must be tested for streaming chat workloads before selecting it as the production default.

### Gateway client

The gateway contract remains an external dependency. The client must support idempotent register, inspect, refresh, and unregister operations. If registration fails while inference is healthy, the runtime remains running and registration retries with bounded exponential backoff. Status must display the split state clearly.

Do not write directly to the gateway's database or configuration files from the RunPod machine. The preferred integration is an authenticated gateway management API. If the current product only supports config/admin updates, a narrow gateway-side registration endpoint should be added there.

### State store

IOLD needs local ownership state even if the gateway uses PostgreSQL. Recommended v0 storage is a small SQLite database at `$IOLD_STATE_DIR/iold.db`, defaulting to `/workspace/.iold/iold.db` on RunPod so it can use persistent volume storage. Writes use transactions and restrictive permissions. Deploy and destroy hold a cross-process file lock for their complete lifecycle, including port allocation. Before exec, a `STARTING` intent containing the port and command is persisted; recovery can adopt the uniquely matching process and save its complete identity.

Each deployment stores its immutable Hugging Face commit SHA and its resolved model-cache directory. The cache directory contains an ownership marker tied to the deployment idempotency key; destroy verifies this marker before deletion.

PostgreSQL stores central gateway deployment/registration metadata. Vault stores gateway-side secrets. Vault is not a log store. Runtime logs stay local in v0; structured operational events can later be sent to the gateway.

## 6. Deployment state machine

```text
REQUESTED -> VALIDATING -> DOWNLOADING -> STARTING -> HEALTHY
                                                     |
                                                     v
                                                REGISTERING
                                                /         \
                                             READY    UNREGISTERED_HEALTHY

Any state -> FAILED
Interrupted/live-state mismatch -> CRASHED
READY/FAILED/CRASHED -> DESTROYING -> DESTROYED
```

Each transition is persisted before and after its side effect. Operations carry a stable deployment ID and idempotency key. On startup and unlocked status calls, IOLD reconciles recorded PIDs, process-start identities, ports, processes, endpoints, and gateway records. A dead runtime is persisted as `CRASHED`; status never continues presenting it as healthy.

## 7. Health and readiness

A successful HTTP health response proves process readiness but not useful inference. Therefore:

- continuous liveness: health endpoint
- deployment readiness: `/v1/models` plus one deterministic, minimal-token inference request
- gateway readiness: gateway can reach the endpoint and perform its own probe

The end-to-end inference check is required before the deployment is marked `READY`, even though routine health monitoring remains lightweight.

## 8. Security

- Bind vLLM only to the interface needed by the selected exposure mechanism.
- Generate a unique high-entropy vLLM API key per deployment.
- Do not rely on the vLLM API key alone; official vLLM guidance notes that it does not protect every server endpoint.
- Preferred production ingress is an outbound-only Cloudflare Tunnel plus service-to-service Access policy, or an equivalently restricted network path.
- With RunPod proxy, use application authentication and accept that the origin is publicly reachable through the proxy.
- Read secrets from environment variables or root-readable files, never CLI flags.
- Redact tokens, Authorization headers (including JSON form), and URLs containing credentials before logs are written to disk.
- `.env`, state, databases, downloaded weights, and logs are ignored by Git.
- Pin runtime images by immutable digest and model repositories by revision.

## 9. Destroy semantics

`destroy` is a first-class reliability path:

1. mark deployment `DESTROYING`;
2. stop accepting gateway traffic/unregister;
3. terminate the vLLM process;
4. stop its tunnel if present;
5. delete deployment-specific model and runtime caches;
6. remove logs and local secrets owned by the deployment;
7. preserve only a minimal non-secret tombstone/audit event unless `--purge` is used;
8. mark `DESTROYED`.

IOLD cannot destroy the RunPod Pod or stop its billing because GPU provisioning is out of scope. The CLI must state this prominently after destroy.

## 10. External dependencies still required

- gateway repository and management API/OpenAPI contract
- gateway staging URL and authentication scheme
- first supported Hugging Face model IDs and revisions
- first GPU/VRAM test matrix
- Cloudflare hostname ownership and tunnel provisioning approach

## 11. Research references

- [RunPod Pods overview](https://docs.runpod.io/pods/overview)
- [RunPod port exposure](https://docs.runpod.io/pods/configuration/expose-ports)
- [vLLM Docker deployment](https://docs.vllm.ai/en/latest/deployment/docker/)
- [vLLM OpenAI-compatible server](https://docs.vllm.ai/en/stable/getting_started/quickstart/)
- [vLLM security guidance](https://docs.vllm.ai/en/stable/usage/security/)
- [Cloudflare Tunnel overview](https://developers.cloudflare.com/tunnel/)
- [Cloudflare Tunnel tokens](https://developers.cloudflare.com/tunnel/advanced/tunnel-tokens/)
- [First Unsloth NVFP4 artifact](https://huggingface.co/unsloth/Qwen3.6-35B-A3B-NVFP4-Fast)
