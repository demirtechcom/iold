# IOLD

**Instant Open-source LLM Deployment**

IOLD is an open-source Go CLI that turns a rented RunPod NVIDIA GPU into a secured, OpenAI-compatible model endpoint with one command. It validates the machine, checks whether the model fits the GPU before downloading anything, starts and supervises vLLM, verifies real inference, and exposes the endpoint through the RunPod proxy — protected by a per-deployment API key.

```text
RunPod GPU + any Hugging Face model
        -> fit check (planner)
        -> vLLM start + health + real inference check
        -> authenticated OpenAI-compatible endpoint
```

## Quickstart (on a RunPod vLLM pod)

```bash
curl -fsSL https://raw.githubusercontent.com/demirtechcom/iold/main/scripts/install.sh | sh
iold doctor                             # validate GPU, driver, disk, network
iold plan Qwen/Qwen2.5-7B-Instruct      # will it fit this GPU?
iold deploy Qwen/Qwen2.5-7B-Instruct    # download, start, verify, expose
```

`deploy` prints the endpoint URL, the generated API key, and a ready-to-run `curl` example. Requests without the key are rejected.

## Commands

| Command | Purpose |
|---|---|
| `iold deploy <model> [--replace]` | Deploy a catalog alias or any Hugging Face model |
| `iold plan <model> [--vram GiB]` | Estimate VRAM fit; suggest quantized variants that fit |
| `iold doctor` | Validate GPU, driver, VRAM, disk, and network |
| `iold status [--json]` | Show deployment phase, port, and PID |
| `iold logs [deployment]` | Stream logs with secrets redacted |
| `iold destroy <id>\|--all [--purge]` | Stop the runtime and remove everything IOLD owns |
| `iold models` | List verified catalog entries |

Gated models (Llama, Gemma, …) work when `HF_TOKEN` is set in the environment.

## Status

The full lifecycle — plan, deploy, health and inference readiness checks, status, redacted logs, ownership-safe destroy — is implemented and covered by 113 race-detector-clean tests against fake vLLM/gateway servers. Validation on real RunPod GPU hardware and automatic gateway registration are in progress; see the [task backlog](docs/TASKS.md).

> **Billing warning:** `iold destroy` stops the model, not the Pod. RunPod keeps billing until you stop the Pod in the RunPod console.

## Documentation

- [Architecture](docs/ARCHITECTURE.md) — system boundary, state machine, security model
- [Implementation plan](docs/PLAN.md) — phased delivery plan
- [Decision log](docs/DECISIONS.md) — accepted and superseded decisions
- [Testing strategy](docs/TESTING.md) — test layers and failure-path matrix
- [Task backlog](docs/TASKS.md) — current status by milestone

## Security model (short version)

- Every deployment gets a unique high-entropy API key, passed to vLLM via environment (never argv), stored with `0600` permissions, deleted on destroy.
- Logs pass through a redaction layer that masks tokens, `Authorization` headers, and credentialed URLs.
- Deployment IDs are validated as single path segments; destroy refuses to touch paths or processes it does not own.
- See [SECURITY.md](SECURITY.md) for reporting vulnerabilities.

## Explicit non-goals

- GPU provisioning or marketplace selection (you rent the Pod)
- Kubernetes or k3s
- Fine-tuning, LoRA, or training
- Multi-cloud support
- General-purpose MLOps

## License

[Apache-2.0](LICENSE)
