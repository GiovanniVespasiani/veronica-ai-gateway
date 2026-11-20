# Threat model — veronica-ai-gateway (Phase A)

Scope: the gateway process, its configuration, and its network position.
Out of scope: n8n itself, node compromise, provider-side breach, sandbox escape.

## Assets

| Asset | Why it matters |
|---|---|
| OpenCode Go API key | Direct financial and reputational impact |
| Workflow/prompt content | May contain internal automation logic and credentials in expressions |
| Response integrity | Assistant output can steer code executed in the n8n sandbox |
| Availability of n8n Assistant | Not production-critical, but a hung gateway must not hang n8n |
| Go monthly allowance | Shared, unmetered per user |

## STRIDE summary

| Category | Threat | Control | Status |
|---|---|---|---|
| Spoofing | Caller pretends to be n8n | ClusterIP-only Service, NetworkPolicy ingress from the n8n main pod, bearer token | Implemented; netpol untested |
| Spoofing | Gateway pretends to be OpenCode's own client | Sends its own User-Agent; never claims to be another product | Implemented |
| Tampering | Request body rewritten to a different model | Single-model allowlist, body forwarded byte-for-byte | Tested |
| Tampering | Client-supplied session header accepted | Header overwritten, never read except in testmode | Tested |
| Repudiation | No record of who called | Per-request `request_id` in logs and `X-Request-Id` response header | Tested |
| Information disclosure | Prompt logged | Fixed-schema logger; sentinel test | Tested |
| Information disclosure | Token logged | Tokens never enter a log field | Tested |
| Information disclosure | Provider key exposed to n8n | Key exists only in the `ai-gateway` namespace | Design; not runtime-verified |
| Denial of service | Oversized body / slow client | 4 MiB cap, header cap, write deadlines | Tested (body) |
| Denial of service | Request flood | Token bucket + in-flight cap | Tested |
| Elevation of privilege | Pod reaches cluster/LAN/metadata | No SA token, no RBAC, egress excludes private/CGNAT/link-local | Design; netpol untested |
| Elevation of privilege | SSRF via model URL / path / Host | Fixed destination, closed routes, `Proxy:nil`, no redirects | Tested (routes, redirect) |

## Explicitly evaluated attacks

1. **Arbitrary destination.** Not configurable. `Production()` hardcodes the base
   URL; environment variables named `GATEWAY_UPSTREAM_URL`, `OPENAI_BASE_URL`,
   etc. are ignored (tested). Only a `-tags testmode` binary reads
   `GATEWAY_TEST_UPSTREAM_URL`.
2. **Session forgery.** A client-supplied `x-opencode-session` is discarded and
   replaced by the resolver's value (tested).
3. **Header smuggling.** Client headers are not copied. `X-Forwarded-For`,
   `X-Real-Ip`, `Host` are dropped or pinned (tested).
4. **Absolute-form request line.** Rejected with 400 before routing; the
   upstream is never contacted (tested).
5. **Redirect to attacker host.** Refused; surfaces as 502 (tested).
6. **Cross-conversation key reuse.** The mock resolver proves stable-per-hint and
   distinct-across-hints semantics; the production resolver refuses to guess.
7. **Log leakage.** A sentinel prompt and both tokens are asserted absent from
   the access log (tested).
8. **Model substitution.** Any model other than `deepseek-v4.1-flash` is rejected
   before the upstream call (tested).

## Security findings recorded during Phase A

| # | Finding | Severity | Disposition |
|---|---|---|---|
| F1 | A production build cannot serve traffic at all: the session resolver fails closed. | Expected | Documented as blocker B2; 501 is deliberate, not a bug |
| F2 | Body buffering allocated 2.16x the body size (`io.ReadAll` growth). | Medium | **Resolved in B1.** Pre-sized read: 1.00x. 16 MiB x8 RSS measured 144.4 MiB (was ~276 MiB inferred). Default lowered to 4 MiB with a 192 MiB config guard. |
| F3 | Rate limiting is global, not per user. n8n RBAC cannot be mapped to the gateway. | Medium | Accepted for a single-tenant instance; documented |
| F4 | The gateway has no per-request cost accounting; a runaway workflow can consume the shared Go allowance. | Medium | Mitigated by rate limit and total timeout only |
| F5 | `Retry-After` relayed verbatim. | Low | **Resolved in B1.** Clamped to 1..600 s; dropped if unparseable. Tested. |
| F6 | The model-list route forwards the upstream response body unfiltered (4 MiB cap). | Low | Read-only; no secret exposure |
| F7 | Access log records `session_prefix`, which is derived from a synthetic/test ID now and would be a provider ID later. Truncated to 16 chars; not a secret, but it is a correlator. | Info | Accepted; documented |
| F8 | `logging.AccessRecord` has a `Message` field that could carry text if misused. Only static strings are assigned today. | Low | Keep as-is; reviewers must reject anything dynamic |
| F9 | No image signature; SBOM is hand-generated; no OS-package scan. | High (pre-deploy) | Image built and hardened; SBOM produced by `scripts/sbom.sh`; `govulncheck` clean. Signing and OS-package scanning remain open (B7). |
| F10 | The initial image was built with Go 1.24.13, while `govulncheck` reported 11 reachable stdlib vulnerabilities in 1.27.1 (fixed in 1.27.2–1.27.4). | **High** | **Resolved.** Dockerfile pinned to `golang:1.27.4-bookworm`; rescan reports none. Any future Go bump must re-run the scan. |
| F11 | n8n retries a failed Assistant call three times. A gateway outage therefore triples upstream attempts once traffic flows. | Low | Documented; rate limit and in-flight cap bound the amplification |
| F12 | Without a sandbox, model output that drives code execution is not isolated. | High (product) | Not a gateway defect; documented in `docs/N8N-COMPATIBILITY.md` §6 and blocker B4 |

## Untested security assumptions

These are stated as assumptions, not facts:

1. **NetworkPolicy enforcement.** Manifests were authored, not applied. Whether
   kubelet probes are blocked by `default-deny`, and whether Calico honours the
   `except` list exactly as written, is unverified.
2. **Distroless runtime.** *Exercised in B1*: `--read-only`, uid 65532, cap-drop
   ALL, no-new-privileges, pids-limit 64, memory 256m — healthz/readyz 200 and
   fail-closed 501 observed.
3. **TLS trust store.** The image relies on distroless CA certificates; not
   exercised because Phase A never made a TLS connection.
4. **Real provider behaviour.** No live call was made. Header shape acceptance,
   `reasoning` parameter acceptance, Chat Completions semantics for this model,
   and `400 MissingSessionID` behaviour are all unverified. The real client
   payload is now known (no reasoning field is sent by n8n: the body contains
   only `messages`, `model`, `stream`, `tool_choice`, `tools`).
5. **n8n's fallback in a real process.** Only the predicate was pinned by test.
6. **Body-size headroom.** *Addressed in B1*: real payloads measured 112-187 KB,
   default lowered to 4 MiB (20x headroom). Very long conversations could still
   exceed it; the 413 is explicit and logged.
7. **DNS.** The policy allows DNS to the kube-dns pods by label. If the cluster
   uses a different resolver label, egress breaks.
