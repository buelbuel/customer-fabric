# Webhook signing secrets and their keys

Every webhook destination has a 32-byte HMAC secret that signs each delivery (`CF-Signature`). The recipient holds the same secret. Customer Fabric stores it sealed with AES-256-GCM under a deployment key. The sealed value is bound to its destination and secret version, so it cannot be copied to another row. Without the key, a database or backup copy does not reveal the secret.

## The key

- **Format.** `CF_WEBHOOK_SECRET_KEYS` holds comma-separated `id:hex` pairs: a key ID (`[a-z0-9][a-z0-9_-]{0,31}`) and 32 random bytes in hex. At most 10 pairs. The first key seals; every listed key opens what it sealed.
- **Production provisioning.** Set `CF_WEBHOOK_SECRET_KEYS_FILE` to a file that your secret manager mounts read-only into the API container, such as a Kubernetes or Docker secret, or a file a KMS-backed agent renders. Never both variables. Generate the key inside the secret manager or with `openssl rand -hex 32` on a trusted host. Never commit it, put it in an image, or log it. Grant read access to the API's runtime identity only, and audit access in the secret manager.
- **Key check.** The first time a key ID seals anything, a check value sealed under that key is recorded in `sealing_keys`. If a different key is later configured under the same ID, it fails the check. It is then reported in `wrong_keys`, and nothing is sealed with it.
- **Local development.** `make m1-up` creates the ignored `.env.webhooks` once, with a `dev-local` key, and never overwrites it. It seals only that stack's synthetic secrets. If you delete it while keeping the stack's database, sealed secrets cannot be opened. Restore the file, or rotate those destinations.

## What happens without the right key

With webhook hosts configured but no key, the API refuses to start. With a wrong or removed key, the API still starts and serves everything else, and it logs that signing secrets need action. `GET /v1/operator/destination-secrets` (activation operator) reports:

- `signing_available` and `rotation_complete`;
- `wrong_keys`: configured key IDs whose key does not match the one first registered under that ID;
- `unavailable`: destinations whose newest secret the configured keys cannot open;
- `stale`: destinations with a live secret sealed under a key other than the sealing key.

It never includes a secret.

For a destination whose secret cannot be opened:

- **Nothing is sent.** Nothing is sent unsigned or with another secret.
- **Activation deliveries wait.** They are retried with reason `signing_key_unavailable`, within their attempts, and can be redriven.
- **Change notifications wait.** The activity is retried until the key is back.
- **Nothing stored changes.** Nothing is resealed, overwritten, regenerated or deleted.
- **Registration and rotation are refused** with 503 while the sealing key is wrong; rotation is also refused for a destination whose current secret does not open.

Configure the key that sealed the secrets, and sending resumes.

## Rotating the deployment key

1. Generate a new key and put it first: `CF_WEBHOOK_SECRET_KEYS=new:...,old:...`. Deploy.
2. The housekeeper reseals every secret under the new key in the background, each one opened first. A secret that does not open is left as it is.
3. When the status reports `rotation_complete`, remove the old key and deploy again. Anything still sealed under a removed key shows as `unavailable` and waits. Put the key back to recover.

Rotation needs no downtime, and recipients notice nothing.

## Rotating a destination's secret

`POST /v1/operator/destinations/{name}/signing-secret` with `overlap_seconds` issues a new secret, returned only in that response. During the overlap, deliveries carry one `v1` per live secret, so the recipient can switch at any time. Afterwards the previous secret is erased. `overlap_seconds: 0` erases the current secret at once, for a secret that leaked. Each rotation is in the operator audit.

## Backup, restore and disaster recovery

- **Back up the keys separately from the database,** in the secret manager, with its own access control and retention. A database backup alone cannot sign; a key alone reveals nothing.
- **Keep every key ID that sealed secrets in a backup** for as long as that backup can be restored. A restored database may need a key already rotated out.
- **To restore,** restore the database and configure the keys it needs: the sealing key first, then older ones. Then check the status. `unavailable` destinations name exactly what a missing key affects.
- **If a key is lost for good,** the secrets it sealed cannot be recovered. Do not revoke or replace them unilaterally. Issuing new secrets for those destinations, and telling their recipients, needs the maintainer's explicit approval.
