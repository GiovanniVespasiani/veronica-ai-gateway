# Rollback

The gateway is stateless; the client is not. A client upgrade may run database
migrations, so rollback depends on what changed.

## Gateway only

```bash
kubectl delete namespace ai-gateway
```

No data is lost. Revoke the provider key at the provider if the deployment is
abandoned.

## Client configuration only

Revert the environment values (or `helm rollback <release> <revision>`) and
restart the client. The gateway can stay up unused.

## Client image upgrade

n8n ships forward-only migrations. There is no down-migration path, so a
downgrade after migrations have run requires restoring the database:

1. Stop the client.
2. Restore the pre-upgrade volume snapshot (Longhorn or equivalent) or the file
   copy you took.
3. Deploy the previous image digest.
4. Verify the database version matches the image.

Take the backup **before** starting the upgraded image, not after.

## Verification after rollback

- Client pod Ready, previous image digest.
- Assistant returns to its previous behaviour.
- No orphaned gateway objects remain.
- Provider key rotated if exposure is suspected.

## Data

Never delete the client's PVC as part of a rollback. The gateway has no
persistent state and no backup requirement.
