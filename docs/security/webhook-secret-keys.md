# Webhook signing secrets and their keys

Every webhook destination has a 32-byte HMAC secret that signs each delivery (`CF-Signature`). The recipient holds the same secret. Customer Fabric stores it sealed with AES-256-GCM under a deployment key. The sealed value is bound to its destination and secret version, so it cannot be copied to another row. Without the key, a database or backup copy does not reveal the secret.

## The key

- **Format.** `CF_WEBHOOK_SECRET_KEYS` holds comma-separated `id:hex` pairs: a key ID (`[a-z0-9][a-z0-9_-]{0,31}`) and 32 random bytes in hex. At most 10 pairs. The first key seals; every listed key opens what it sealed.
- **Production provisioning.** Set `CF_WEBHOOK_SECRET_KEYS_FILE` to a file that your secret manager mounts read-only into the API container, such as a Kubernetes or Docker secret, or a file a KMS-backed agent renders. Never both variables. Generate the key inside the secret manager or with `openssl rand -hex 32` on a trusted host. Never commit it, put it in an image, or log it. Grant read access to the API's runtime identity only, and audit access in the secret manager.
- **Key check.** The first time a key ID seals anything, a check value sealed under that key is recorded in `sealing_keys`. If a different key is later configured under the same ID, it fails the check. It is then reported in `wrong_keys`, and nothing is sealed with it.
- **Local development.** `make m1-up` creates the ignored `.env.webhooks` once, with a `dev-local` key, and never overwrites it. It seals only that stack's synthetic secrets. If you delete it while keeping the stack's database, sealed secrets cannot be opened. Restore the file, or rotate those destinations.

## What happens without the right key

The API still starts and serves everything else, and it logs that signing secrets need action. `GET /v1/operator/destination-secrets` (activation operator) reports:

- `signing_available`;
- `wrong_keys`;
- each destination as `plaintext`, `sealed`, `unfinalized`, `unavailable` or `stale`.

Its secrets are never included.

For a destination whose secret cannot be opened:

- **Nothing is sent.** Nothing is sent unsigned or with another secret.
- **Activation deliveries wait.** They are retried with reason `signing_key_unavailable`, within their attempts, and can be redriven.
- **Change notifications wait.** The activity is retried until the key is back.
- **Nothing stored changes.** Nothing is resealed, overwritten, regenerated or deleted.
- **Registration and rotation are refused** with 503.

Configure the key that sealed the secrets, and sending resumes.

## Upgrading from plaintext secrets

Migration 031 keeps every existing plaintext secret. Then:

1. **Configure a key** and deploy. The API's housekeeper seals each plaintext secret as the same value. It checks that the sealed value opens and signs exactly as the plaintext does, and keeps the plaintext. Each batch is one transaction, so an interrupted run leaves every destination either untouched or sealed, and it continues on the next run. `api secrets migrate` runs the same step at once. Recipients notice nothing: the signature is unchanged.
2. **Check** `api secrets status` or the endpoint. `plaintext` must be empty and `signing_available` true. Destinations listed under `unfinalized` still keep their plaintext.
3. **Finalize** with `api secrets finalize`. For each destination, under its lock, it opens the sealed secret again and compares signatures with the plaintext. Only then does it clear the plaintext. A destination that does not verify keeps its plaintext and is listed under `refused`. The command can be repeated.

Before step 3, the previous release can run against the database unchanged. Destinations registered or rotated by this release have no plaintext secret, though.

## Rolling back

1. Run `api secrets rollback` with the current keys. It restores each destination's newest secret as plaintext. A destination whose newest secret does not open is listed under `refused` and left unchanged.
2. Deploy the previous release.
3. Run the down migration. It refuses while any destination lacks a plaintext secret, so it never drops the only copy of a secret.

The rollback command is the only path that writes a secret in the clear, and only on an explicit downgrade.

## Rotating the deployment key

1. Generate a new key and put it first: `CF_WEBHOOK_SECRET_KEYS=new:...,old:...`. Deploy.
2. The housekeeper reseals every secret under the new key in the background, each one opened and verified first. A secret that does not open is left as it is.
3. When the status reports `rotation_complete`, remove the old key and deploy again. Anything still sealed under a removed key shows as `unavailable` and waits. Put the key back to recover.

Rotation needs no downtime, and recipients notice nothing.

## Rotating a destination's secret

`POST /v1/operator/destinations/{name}/signing-secret` with `overlap_seconds` issues a new secret, returned only in that response. During the overlap, deliveries carry one `v1` per live secret, so the recipient can switch at any time. Afterwards the previous secret is erased. `overlap_seconds: 0` erases the current secret at once, for a secret that leaked. Each rotation is in the operator audit.

## Backup, restore and disaster recovery

- **Back up the keys separately from the database,** in the secret manager, with its own access control and retention. A database backup alone cannot sign; a key alone reveals nothing.
- **Keep every key ID that sealed secrets in a backup** for as long as that backup can be restored. A restored database may need a key already rotated out.
- **To restore,** restore the database and configure the keys it needs: the sealing key first, then older ones. Then check the status. `unavailable` destinations name exactly what a missing key affects.
- **If a key is lost for good,** the secrets it sealed cannot be recovered. Do not revoke or replace them unilaterally. Issuing new secrets for those destinations, and telling their recipients, needs the maintainer's explicit approval.
