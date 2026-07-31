# ADR-0008 — Envelope-encrypted secrets in Postgres

- **Status:** Accepted
- **Date:** 2026-07-24
- **Deciders:** @nickemma

## Context

Veyronix holds the credentials that deploy to every target an organization owns: Netlify tokens, SSH private keys, Heroku API keys, kubeconfigs, cloud access keys. It is, by construction, one of the highest-value credential stores in the organization.

The design assumption must therefore be that **the database will eventually leak** — through a backup on an unsecured bucket, a read-replica misconfiguration, a SQL injection, or a laptop. A design that is only safe while Postgres is safe is not a design.

## Decision

**Envelope encryption**, AES-256-GCM, with KEK/DEK separation.

- Each project gets a **DEK** (data encryption key), generated at project creation.
- Secrets are encrypted with the DEK. Each ciphertext stores its own nonce; project and secret IDs are bound in as additional authenticated data, so a ciphertext cannot be moved between projects.
- The DEK is stored **wrapped by a KEK** that lives outside Postgres — `VEYRONIX_KEK` in V1, a cloud KMS or Vault transit key later. The interface is `crypto.Sealer`, so the KEK source is an adapter, not a rewrite.
- A database dump alone yields ciphertext and wrapped DEKs, and nothing usable.

Handling rules, which matter as much as the cryptography:

- Secrets are decrypted **only in worker memory**, at deploy time, immediately before the provider call.
- Never written to disk. Never included in the event stream. Never in build logs.
- Log lines are scrubbed against known secret values before emission — a build script that echoes its environment must not leak through the log pipeline.
- Build containers get no access to worker credentials and no network egress by default.
- Rotation re-wraps DEKs under a new KEK without re-encrypting every secret; secret rotation is separate and per-secret.

## Alternatives Considered

**HashiCorp Vault as the store.** The correct long-term answer: dynamic secrets, leasing, revocation, and audit are all better than anything built here. Rejected for V1 as a hard dependency — Vault is a serious system to run well, and running it badly is worse than not running it. The `Sealer` interface leaves the door open: Vault transit becomes an adapter.

**Cloud KMS only (AWS KMS, GCP KMS).** Rejected as the sole mechanism: it ties a deliberately provider-agnostic platform to one cloud, and a KMS call per secret read puts a network dependency on the deploy hot path. KMS is a good KEK source, which is exactly how it is used.

**Postgres `pgcrypto` with a key in the database.** Rejected: a key stored beside the ciphertext it protects is not encryption, it is obfuscation.

**Full-disk / at-rest encryption only.** Rejected: protects against a stolen disk, not against a leaked dump, a compromised replica, or a `SELECT` by an over-privileged service account — which are the realistic threats.

**Not storing secrets at all; user brings credentials per deploy.** Rejected: it defeats CI-triggered and scheduled deployments, and pushes the credential problem back to the individual developers the platform exists to relieve.

## Consequences

**Positive**

- Database compromise alone does not compromise credentials.
- Blast radius of a leaked DEK is one project.
- Rotation is cheap at the KEK layer.
- The scrubbing and no-disk rules make credential exposure through logs a designed-against case rather than a discovered one.

**Negative**

- KEK management becomes the critical operational responsibility. **Losing the KEK means losing every secret** — this needs a documented, tested backup and recovery procedure before the first real credential is stored.
- Decryption in worker memory means a worker core dump or a memory-scraping compromise is a credential exposure. Mitigated by process isolation and short-lived decryption windows, not eliminated.
- Log scrubbing is best-effort against known values. A secret transformed by user code (base64-encoded, concatenated, echoed in pieces) can defeat it. This is a known, accepted limitation and is documented in the [threat model](../THREAT_MODEL.md).
- No dynamic or short-lived credentials in V1 — a stored provider token is valid until manually rotated.

## Revisit If

- Vault or a cloud KMS becomes available in the target environment → move the `Sealer` adapter, keep everything else.
- Providers begin supporting OIDC federation or short-lived credentials, at which point storing long-lived tokens becomes avoidable and should be avoided.
