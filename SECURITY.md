# Security policy

## Reporting

Open a private security advisory on this repository. Do not open a public issue
for a vulnerability. Please include a minimal reproduction and the affected
version.

## Secrets

This project never needs a provider key in source, tests or images. The gateway
reads it from a file mounted from a Kubernetes Secret. `scripts/check-build-context.sh`
runs in CI and refuses to build if the context looks like it contains
credentials.

If you believe a key was committed, rotate it at the provider first, then
rewrite history.

## Threat model (summary)

| Threat | Control |
|---|---|
| Provider key exfiltration via the client | Key exists only in the gateway namespace |
| SSRF / arbitrary destination | Compile-time upstream constant, closed routes, no redirects, `Proxy: nil` |
| Prompt or token leakage in logs | Fixed-schema access records; no bodies |
| Session mixing | Strict header identity, hashed to an opaque value, fail closed |
| DoS / runaway usage | Body, rate, concurrency and timeout limits |
| Lateral movement from the gateway | No service-account token, egress limited to DNS + 443 excluding private ranges |

Full model: `docs/THREAT-MODEL.md`.

## Scope and non-goals

The gateway is not a sandbox and does not protect against a compromised n8n
instance or a compromised node. It assumes the cluster network is trusted for
the single allowed hop, and that only the client pod can reach the Service.

## Supported versions

Security fixes are applied to the current minor line. The gateway image is
rebuilt from a pinned toolchain and rescanned on every change.
