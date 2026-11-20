# Deployment (example)

These steps describe a generic single-cluster deployment. They are examples, not
a supported procedure for any specific environment.

## 1. Build and publish

```bash
docker build --platform linux/amd64 -t ghcr.io/<owner>/veronica-ai-gateway:<tag> .
```

Publish with the provided workflow (manual dispatch, reviewer-gated), then record
the immutable digests.

## 2. Secrets

Two independent secrets, in two namespaces:

```bash
# gateway namespace
kubectl create secret generic ai-gateway-client   --from-literal=client-token="$(openssl rand -hex 32)"
kubectl create secret generic ai-gateway-provider --from-literal=provider-key="<provider key>"

# client namespace: only the low-value gateway token
kubectl create secret generic ai-gateway-client-token --from-literal=client-token="<same client token>"
```

Never place the provider key in the client namespace.

## 3. Gateway

```bash
kubectl apply -k deploy/ai-gateway
kubectl rollout status deploy/ai-gateway
```

Replace the example image reference with your digest first. If your registry is
private, attach a pull secret to the Deployment (and to the client's
ServiceAccount if that chart has no pull-secret value).

Verify the NetworkPolicy by confirming that a pod **outside** the allowed source
cannot reach the Service.

## 4. Client

Configure the assistant client with:

```
MODEL_URL=http://ai-gateway.<namespace>.svc.cluster.local:8080/v1
MODEL=custom/<model-id>
MODEL_API_KEY=<client token>
```

The client must send `x-n8n-thread-id`. Without it, every request is rejected
with `501` by design.

## 5. First run

1. Send one short message; expect `200`.
2. Check the gateway log: one access record, no prompt or token.
3. Send one tool-using turn.
4. Stop on any 4xx, unexpected billing, or a session mismatch.

## Preconditions

- Provider acceptable-use confirmed for your traffic.
- Client patch validated against your exact n8n version.
- Database backup taken before changing the client.
