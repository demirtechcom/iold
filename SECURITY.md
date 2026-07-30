# Security Policy

## Supported versions

Only the latest release receives security fixes.

## Reporting a vulnerability

Email suleyman.demir@demirtech.com with a description, reproduction steps, and impact assessment. Do not open a public issue for security problems. You will receive an acknowledgement within 72 hours.

## Scope notes

IOLD manages GPU-backed inference processes and gateway registrations. Reports we consider especially critical:

- secrets (tokens, API keys) leaking into logs, state, error messages, or gateway payloads
- path traversal or deletion outside IOLD-owned directories during `destroy`
- signals delivered to processes IOLD does not own (PID confusion)
- endpoint exposure beyond the selected exposure mechanism
- checksum or release-verification bypass in the install script
