# veronica-ai-gateway

A minimal, closed-routing OpenAI-compatible gateway for an internal assistant
(originally built for n8n Assistant) that must talk to a provider which requires
a stable per-conversation identifier and must never receive the provider key from
the assistant pod.

## What it does

```
assistant client --http--> ai-gateway (ClusterIP) --https--> provider
```

- **Closed routing**: four paths. No generic proxy, no redirect following, no
  ingress.
- **Strict conversation identity**: the client must send `x-n8n-thread-id`
  (or `x-conversation-id`). The gateway hashes it into an opaque value and uses
  it as the provider session header. Missing or malformed → `501`, no outbound
  call. There is no content-derived fallback, no global id and no per-request
  randomness.
- **Key isolation**: the provider key exists only in the gateway namespace.
  The client holds a separate, low-value token.
- **Fixed destination**: the upstream URL is a compile-time constant. Environment
  proxies are ignored, redirects are refused, TLS is verified.
- **Metadata-only logs**: never prompts, headers, tokens or full session ids.
- **Limits**: 4 MiB body, 300 req/min, burst 60, 8 concurrent, 120 s stream idle,
  192 MiB memory budget guard.

## Client contract

Every model request MUST carry:

```
x-n8n-thread-id: <stable per-conversation identifier>
```

The identifier must be stable for the life of a conversation. The gateway
derives `x-opencode-session` from it with SHA-256, so the raw value never leaves
the cluster.

If your client cannot send that header yet, the gateway can run with
`GATEWAY_SESSION_MODE=unavailable`, which fails every chat request closed.

## Quick start (no network, no cluster)

```bash
go test ./...
go test -race ./...
./tests/integration.sh   # loopback mock upstream + curl assertions
```

## Deploying

See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) and
[docs/ROLLBACK.md](docs/ROLLBACK.md). The manifests in `deploy/ai-gateway/` are
examples: replace the image reference and review every value before applying.

## Limitations

- The reference client integration required a small client-side change to emit
  the conversation header; that change is maintained separately and is not part
  of this repository.
- Sandbox-dependent assistant features are out of scope.

## License

MIT. See `LICENSE`.
