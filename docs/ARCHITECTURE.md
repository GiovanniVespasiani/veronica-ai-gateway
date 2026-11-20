# Architecture — veronica-ai-gateway (Phase A)

## 1. Context

```
n8n Assistant (ns n8n, ClusterIP <cluster-ip>:5678)
        │  HTTP POST /v1/chat/completions
        │  Authorization: Bearer <gateway client token>
        ▼
Service ai-gateway.ai-gateway.svc:8080        (ClusterIP only)
        │
        ▼
Deployment ai-gateway  (1 replica, no SA token, read-only rootfs)
        │  HTTPS 443, TLS verified, fixed host, no proxy, no redirects
        ▼
https://opencode.ai/inference/go/openai/v1/chat/completions
```

No Ingress, NodePort, LoadBalancer, Tailscale route or Gateway API object is
defined.

## 2. Request lifecycle

1. `ServeHTTP` assigns `req_X` and rejects absolute-form request targets.
2. Exact-path routing (closed table). Anything else is 404.
3. `POST /v1/chat/completions`:
   a. bearer authentication (SHA-256 digests, constant-time compare);
   b. token-bucket rate limit (sustained + burst);
   c. non-blocking in-flight semaphore (bounded concurrency);
   d. body read through `http.MaxBytesReader`;
   e. minimal JSON decode: `model`, `stream`;
   f. model allowlist is a single equality check;
   g. session resolver invoked;
   h. upstream request built from a fixed header set;
   i. streaming relay with per-write deadlines and per-chunk flush.
4. One access record is emitted. No body, header, or token is ever passed to it.

## 3. Route table (complete)

| Method | Path | Result |
|---|---|---|
| GET | `/healthz` | 200 `{"status":"ok"}` |
| GET | `/readyz` | 200 `{"status":"ready"}` |
| POST | `/v1/chat/completions` | proxied, SSE passthrough |
| GET | `/v1/models` | proxied (validation helper) |
| POST | `/v1/responses` | **404** `error.code=unknown_url` |
| any | anything else | 404 `not_found` |

## 4. The Responses → Chat Completions contract

n8n Assistant (n8n 2.48.5, `@n8n/instance-ai` 1.33.4, `@n8n/agents` 0.33.4) uses
the OpenAI **Responses** API by default and falls back to Chat Completions when
it detects a missing Responses route. The detection accepts either
`error.code === "unknown_url"` or a message matching
`/^Unknown request URL: \S+ \/(?:[^/\s]+\/)*responses\b/i`.

The gateway returns exactly:

```json
{"error":{"message":"Unknown request URL: POST /v1/responses","type":"invalid_request_error","code":"unknown_url","param":""}}
```

so the fallback is deterministic and no request body is translated. The predicate
is pinned by `TestResponsesRouteTriggersN8nChatFallback`.

**Not verified in Phase A:** the real n8n process exercising the fallback. That
requires the n8n image, a database and — for Assistant to actually work — a
sandbox. See `docs/BLOCKERS.md` B3/B4 and `docs/TEST-RESULTS.md`.

## 5. Session identity (replaceable by design)

```go
type Resolver interface {
    Resolve(ctx context.Context, meta RequestMeta) (ID, error)
    Describe() string
}
```

| Implementation | Where | Behaviour |
|---|---|---|
| `session.Unavailable` | production (fail-closed) | returns `ErrNoReliableConversationID`; request is refused with 501 |
| `session.HeaderResolver` | **production (approved)** | `ses_` + sha256(`x-n8n-thread-id`); strict, no fallback |
| `session.DerivedResolver` | tests only | content-derived; refused in production |
| `session.NewMockForTests` | tests + `testmode` build | maps an explicit conversation hint to `ses_mock_NNNNNN` |
| `session.NewCaptureForTests` | `testmode` + `protocol-capture` mode | one synthetic ID per request; observation only, explicitly not a solution |

`ID.Valid()` enforces the documented `^ses_[A-Za-z0-9_-]{4,120}$` shape before a
request is sent, so a malformed resolver can never emit a bad header.

Prefix-hash derivation is **not implemented**. Adding it requires an approved
decision (blocker B2), because it can mislabel distinct conversations that share
a message prefix.

## 6. Upstream client invariants

- `Proxy: nil` — `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` are ignored.
- `CheckRedirect` returns `ErrUseLastResponse`; any 3xx becomes
  `502 upstream_redirect`.
- TLS `MinVersion = TLS 1.2`, system roots, no `InsecureSkipVerify` field exists.
- Endpoint path is rebuilt from the validated base URL; the final host is compared
  against the configured host before the request is issued.
- Only `Content-Type`, `Accept`, `User-Agent`, `Authorization` and
  `x-opencode-session` are sent. `req.Host` is pinned.
- Response headers forwarded to n8n: `Content-Type` and `Retry-After` only.
  `Cache-Control: no-store` is always added.

## 7. Limits (defaults)

| Limit | Default | Mechanism |
|---|---|---|
| Body size | 4 MiB | `http.MaxBytesReader` → 413 |
| Body allocation | 1x body (+1 byte) when `Content-Length` is present | pre-sized buffer, avoids `io.ReadAll` growth |
| `Retry-After` relay | clamped to 1..600 s, dropped if unparseable | `clampRetryAfter` |
| Header size | 32 KiB | `http.Server.MaxHeaderBytes` |
| Connect timeout | 5 s | dialer |
| Response headers | 180 s | transport |
| Stream idle (write) | 120 s | `ResponseController.SetWriteDeadline` |
| Total request | 900 s | `context.WithTimeout` |
| In-flight | 8 | buffered channel → 503 + `Retry-After` |
| Memory budget | `MaxInFlight * MaxBodyBytes <= 192 MiB` | rejected at config validation |
| Rate | 30/min, burst 10 | token bucket → 429 + `Retry-After` |
| Response body (models) | 4 MiB | `io.LimitReader` |
| Error body relay | 64 KiB | `io.LimitReader` |

## 8. Measured memory

`tests/memory.sh` drives the real binary against the loopback mock and samples
RSS. At 16 MiB x 8 concurrent: 12.6 MiB baseline, **144.4 MiB peak** (~1.03x per
in-flight body). At the shipped 4 MiB x 8: **49.3 MiB peak**. The 4 MiB default
was chosen because the real Assistant payloads measured 112-187 KB in
`docs/N8N-COMPATIBILITY.md`.

## 9. Kubernetes manifests (review only)

`deploy/ai-gateway/` contains Namespace, ServiceAccount (no bindings, no token),
Deployment (non-root 65532, read-only rootfs, all caps dropped, RuntimeDefault
seccomp, limits, probes), ClusterIP Service, PDB, and two NetworkPolicies:
namespace default-deny plus an allow-list for ingress from the n8n main pod and
egress to DNS and TCP/443 with private/CGNAT/link-local ranges excluded.

They have **not** been applied and have **not** been validated by a cluster-side
dry run.
