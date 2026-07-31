# Veyronix — Operational Runbook

> **How to use this document.** Every procedure follows the same shape: **Symptom → Assess → Act → Verify → Follow-up**. Under pressure, read the assess step and do not skip it — most bad incidents get worse because someone acted on an assumption.
>
> If you are paged, you are here for [Rollback failure](#rollback-failure). That is the only condition that pages a human.

| | |
|---|---|
| **Last updated** | 2026-07-28 |
| **On-call** | see rotation |
| **Dashboards** | Grafana → Veyronix / Overview, Veyronix / Queue, Veyronix / Providers |
| **Escalation** | Platform owner → engineering lead |

---

## First five minutes

Before anything else, establish these four facts:

```bash
# 1. Is the control plane up?
curl -sf https://veyronix.internal/healthz && echo OK

# 2. What is the job queue doing?
psql "$VEYRONIX_DATABASE_URL" -c "
  SELECT state, count(*), max(now() - created_at) AS oldest
    FROM jobs GROUP BY state ORDER BY 2 DESC;"

# 3. Is the outbox draining?
psql "$VEYRONIX_DATABASE_URL" -c "
  SELECT count(*) AS unpublished,
         max(now() - created_at) AS lag
    FROM outbox WHERE published_at IS NULL;"

# 4. Are workers alive?
psql "$VEYRONIX_DATABASE_URL" -c "
  SELECT lease_owner, count(*), max(lease_expires - now()) AS ttl
    FROM jobs WHERE lease_expires > now() GROUP BY 1;"
```

Queue depth flat and outbox lag near zero means the platform is healthy and the problem is a specific deployment or provider. Either number climbing means the problem is the platform.

---

## Severity

| Sev | Definition | Response |
|---|---|---|
| **SEV1** | No deployments or rollbacks possible platform-wide | Page immediately, all hands, status update every 30 min |
| **SEV2** | One provider or environment unable to deploy; rollback unavailable for one environment | Page on-call, update hourly |
| **SEV3** | Elevated latency, degraded observability, single stuck deployment | Business hours |
| **SEV4** | Cosmetic, single-user issue | Ticket |

Rollback being unavailable is at least SEV2 regardless of anything else, because it is the control someone needs during *their* incident.

---

## Rollback failure

**This is the paging condition.** A deployment reached `RollingBack` and could not complete: `RollingBack → Failed`.

### Symptom

Alert `VeyronixRollbackFailed`. A `Failed` deployment whose `previous_id` is set. The environment is now serving a release that failed its health check, and the platform could not restore the previous one.

### Assess

```bash
veyronix history --project "$PROJECT" --env "$ENV" --last 5

psql "$VEYRONIX_DATABASE_URL" -c "
  SELECT id, state, provider, release_id, previous_id, error_class, error_detail
    FROM deployments
   WHERE project_id = '$PROJECT' AND environment = '$ENV'
   ORDER BY created_at DESC LIMIT 5;"
```

Determine which of three cases applies:

| Case | Signal | Meaning |
|---|---|---|
| **A. Target is broken** | Provider `Status` returns error or unknown | The environment may be serving nothing |
| **B. Provider rejected the rollback** | `error_class = provider_error` | Target is likely still serving the bad release |
| **C. Previous release no longer exists** | `error_class = release_not_found` | Provider expired or garbage-collected the artifact |

Check what is actually being served — do not trust the platform's record here:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' "$ENV_HEALTH_URL"
veyronix logs --deployment "$DEPLOYMENT_ID" --tail 200
```

### Act

1. **Freeze the environment** so nothing else lands on top of the mess:
   ```bash
   veyronix env freeze --project "$PROJECT" --env "$ENV" --reason "incident $INCIDENT_ID"
   ```
2. **Case A or B** — deploy the last known-good version explicitly:
   ```bash
   veyronix history --project "$PROJECT" --env "$ENV" --state succeeded --last 1
   veyronix deploy --project "$PROJECT" --env "$ENV" --version "$LAST_GOOD_SHA" --no-auto-rollback
   ```
   `--no-auto-rollback` is deliberate: a failed automatic rollback loop during an incident makes state harder to reason about, not easier.
3. **Case C** — the artifact is gone; a rebuild is required. Deploy from the last-good commit SHA, accept the build time, and communicate that recovery is bounded by build duration.
4. **If the provider itself is down**, this becomes [Provider outage](#provider-outage). Recovery is blocked until it returns; say so in the incident channel rather than retrying silently.
5. **Last resort — manual intervention at the target.** Use the per-provider procedure in [`providers/`](providers/). Record every manual action in the incident timeline; the platform's view of state is now wrong and must be reconciled afterwards.

### Verify

- Health endpoint returns 200 sustained for five minutes.
- `veyronix history` shows a `Succeeded` deployment at the top.
- `veyronix_time_to_recovery_seconds` recorded for the incident.

### Follow-up

Unfreeze the environment. Open a postmortem using [`sre/postmortem-template.md`](sre/postmortem-template.md) — rollback failure is always postmortem-worthy, because rollback is the control everything else depends on.

---

## Deployments stuck in Queued

### Symptom

`veyronix_deployment_queue_depth` climbing; jobs in `queued` with age beyond the 10s queue-time SLO.

### Assess

```bash
# Are any workers holding leases?
psql "$VEYRONIX_DATABASE_URL" -c "
  SELECT lease_owner, count(*) FROM jobs
   WHERE lease_expires > now() GROUP BY 1;"

# Is one environment blocking its own queue?
psql "$VEYRONIX_DATABASE_URL" -c "
  SELECT project_id, environment, state, count(*)
    FROM jobs WHERE state <> 'succeeded'
   GROUP BY 1,2,3 ORDER BY 4 DESC LIMIT 20;"
```

| Finding | Cause |
|---|---|
| No leases held at all | Workers are down or cannot reach Postgres |
| Leases held, queue still growing | Worker starvation — scale out |
| One `(project, environment)` stuck in `deploying` | Concurrency control working as designed; a single long deploy is blocking its own queue |
| Outbox lag high | Relay problem — see [Outbox relay lag](#outbox-relay-lag) |

### Act

- **Workers down** — check pod or service status and worker logs for Postgres connection errors. Restarting is safe: leases expire and jobs resume from the last durable checkpoint.
- **Starvation** — scale worker replicas. `VEYRONIX_WORKER_CONCURRENCY` (default 4) raises per-worker parallelism; prefer more replicas over higher concurrency, since build isolation is the resource constraint.
- **One long deploy blocking an environment** — this is correct behaviour. Confirm the in-flight deploy is progressing (`veyronix logs --follow`), and cancel it only if it has genuinely hung:
  ```bash
  veyronix cancel --deployment "$DEPLOYMENT_ID"
  ```

### Verify

Queue depth falling, oldest queued job age back under 10s.

---

## Worker died mid-deployment

### Symptom

`veyronix_job_lease_expirations_total` increments; a deployment sat in `claimed`, `building`, or `deploying` and then returned to `queued`.

### Assess

This is a **designed-for** event, not an incident by itself. Expected behaviour: the lease expires after `VEYRONIX_JOB_LEASE_TTL` (60s), another worker claims the job, and the provider's idempotency key prevents a duplicate release.

It becomes an incident when it repeats. Check whether one worker is dying repeatedly:

```bash
psql "$VEYRONIX_DATABASE_URL" -c "
  SELECT lease_owner, count(*) FROM job_lease_expirations
   WHERE created_at > now() - interval '1 hour'
   GROUP BY 1 ORDER BY 2 DESC;"
```

### Act

- **Single expiry, job resumed, deploy succeeded** — no action. Note it and move on.
- **One worker repeatedly dying** — cordon it, take a heap profile if it is OOM, check for a provider call leaking memory or a build container escaping its quota.
- **All workers dying** — likely a poison job. Find the deployment they all claimed last, cancel it, and inspect its payload before re-enabling.

### Verify

Deployment reached a terminal state. Confirm no duplicate release exists at the target — the idempotency key should have prevented it, and confirming is how you learn whether it did.

---

## Outbox relay lag

### Symptom

Alert `VeyronixOutboxLag`. Unpublished outbox rows accumulating; the UI shows stale deployment state.

### Assess

```bash
psql "$VEYRONIX_DATABASE_URL" -c "
  SELECT count(*), min(created_at), max(now() - created_at) AS lag
    FROM outbox WHERE published_at IS NULL;"

nats stream info DEPLOYMENTS
```

### Act

- **NATS unreachable** — jobs and events are safe in Postgres; this is degraded, not lost. Restore NATS; the relay drains the backlog automatically in commit order.
- **Relay process down** — restart it. It is stateless and resumes from the first unpublished row.
- **Relay running but slow** — check for a large backlog after an outage (expect a drain period), lock contention on the outbox table, or an oversized outbox table needing pruning:
  ```sql
  DELETE FROM outbox
   WHERE published_at IS NOT NULL
     AND published_at < now() - interval '7 days';
  ```
  Prefer partition drops over bulk `DELETE` once the table is large.

### Verify

Unpublished count returns to near zero; event delivery lag p99 back under 2s.

---

## Provider outage

### Symptom

`veyronix_provider_api_errors_total` spiking for one provider; the circuit breaker has opened after `VEYRONIX_PROVIDER_CIRCUIT_THRESHOLD` consecutive failures.

### Assess

Confirm it is the provider and not us: check the provider's public status page, then try a direct API call with the same credential from outside Veyronix. Distinguish outage from rate limiting — a 429 pattern means back off; a 5xx pattern means wait.

### Act

1. Confirm the blast radius is limited to that provider. Other providers must be unaffected; if they are not, this is a platform incident, not a provider incident.
2. Leave the circuit breaker open. Jobs requeue with exponential backoff and drain on recovery.
3. Communicate to affected teams: deploys to that provider are queued, not lost.
4. If it is a credential problem rather than an outage — expired or revoked token — rotate:
   ```bash
   veyronix target credentials rotate --project "$PROJECT" --target "$TARGET"
   ```

### Verify

Error rate returns to baseline; circuit closes; queued jobs drain.

---

## Postgres unavailable

**SEV1.** The control plane cannot accept mutations.

### Assess

Health endpoint fails. API returns 503 for mutating calls by design; reads may still be served from cache.

### Act

1. In-flight worker jobs continue and buffer their results — do not kill workers. Killing them turns a recoverable degradation into lost work.
2. Restore Postgres (failover to replica, or restore from backup — follow the database runbook).
3. After recovery, reconcile:
   ```sql
   -- jobs whose leases expired during the outage
   SELECT id, state, lease_owner, lease_expires FROM jobs
    WHERE lease_expires < now() AND state NOT IN ('succeeded','failed','rolled_back','cancelled');
   ```
   These resume automatically. Verify they do rather than assuming.

### Verify

Health endpoint green, a test deployment to a non-production environment succeeds end to end, outbox drained.

---

## Secret decryption failure

### Symptom

Deploys failing at pre-flight with `error_class = secret_decryption_failed`. **No provider call was made** — there is no partial state at the target, which is the point of validating before acting.

### Assess

| Cause | Check |
|---|---|
| Wrong KEK loaded | Compare the KEK fingerprint the process reports against the expected one |
| KEK rotated without re-wrapping DEKs | Check the rotation audit record |
| Ciphertext corrupted | AAD mismatch in the error detail |

### Act

- **Wrong KEK** — correct `VEYRONIX_KEK` and restart. Do not re-encrypt anything; the data is fine.
- **Rotation incomplete** — resume the re-wrap job. Do not delete the old KEK until it reports complete.
- **Corruption** — restore the secret from source of truth (the provider's own console) and re-enter it. There is no recovery from corrupted ciphertext.

> **If the KEK is lost entirely, every stored secret is unrecoverable.** This is by design ([ADR-0008](adr/0008-envelope-encryption.md)). Recovery means re-entering every credential from each provider's console. The KEK backup procedure must be tested on a schedule, not assumed.

---

## Duplicate release at a target

### Symptom

Two releases at a provider for one logical deployment.

### Assess

This should be impossible; the idempotency key exists to prevent it. Determine which control failed:

```sql
SELECT id, idempotency_key, release_id, created_at
  FROM deployments
 WHERE project_id = '$PROJECT' AND environment = '$ENV'
 ORDER BY created_at DESC LIMIT 10;
```

Same key on two rows means the API-side check failed. Same key on one row with two provider releases means the **provider** ignored the key — a provider bug, and a conformance suite gap.

### Act

Remove the extra release using the provider's own tooling ([`providers/`](providers/)). Then fix the cause: an API-side failure is a bug in `create_deployment`; a provider-side failure means adding the case to `sdk/conformance` so it cannot recur silently in that provider or any other.

---

## Environment freeze and unfreeze

```bash
veyronix env freeze   --project "$PROJECT" --env "$ENV" --reason "incident $ID"
veyronix env unfreeze --project "$PROJECT" --env "$ENV"
veyronix env status   --project "$PROJECT" --env "$ENV"
```

Freezing rejects new deployments and lets in-flight ones finish. Use it during any incident where the environment's state is uncertain — competing deployments during an incident are how a bad hour becomes a bad day.

---

## Error budget exhaustion

Not an outage, but it triggers a process ([`sre/slo.md`](sre/slo.md)):

- **> 50% consumed** — new provider work stops; reliability work takes priority.
- **100% consumed** — deployments to Veyronix itself require review.

The budget is the forcing function. Overriding it is a decision with a name attached, recorded in the incident channel — not a silent one.

---

## Routine operations

| Task | Command / procedure |
|---|---|
| Deploy Veyronix itself | Through Veyronix, once V1 lands; until then `make deploy` from CI |
| Run migrations | `veyronix-migrate up` — additive-only during rolling deploys |
| Rotate the KEK | `veyronix admin kek rotate --new-kek-file <path>`; re-wraps DEKs, does not re-encrypt secrets |
| Prune outbox | Partition drop, retention 7 days |
| Archive completed jobs | Monthly, to the `jobs_archive` partition |
| Add a provider credential | Dashboard or `veyronix target create`; `Validate` runs synchronously |
| Check what a subject can do | `veyronix authz explain --subject <s> --action <a> --resource <r>` |

---

## Alert reference

| Alert | Condition | Sev | Procedure |
|---|---|---|---|
| `VeyronixRollbackFailed` | `RollingBack → Failed` | SEV2 | [Rollback failure](#rollback-failure) |
| `VeyronixQueueDepthHigh` | depth > 50 for 5m | SEV3 | [Stuck in Queued](#deployments-stuck-in-queued) |
| `VeyronixQueueWaitSLO` | p95 wait > 10s for 10m | SEV3 | [Stuck in Queued](#deployments-stuck-in-queued) |
| `VeyronixOutboxLag` | unpublished > 100 or lag > 60s | SEV2 | [Outbox relay lag](#outbox-relay-lag) |
| `VeyronixProviderErrors` | error rate > 10% for 5m per provider | SEV2 | [Provider outage](#provider-outage) |
| `VeyronixAPIAvailability` | 5xx rate > 1% for 5m | SEV1 | [Postgres unavailable](#postgres-unavailable) or platform triage |
| `VeyronixLeaseExpirations` | > 5 in 10m | SEV3 | [Worker died](#worker-died-mid-deployment) |
| `VeyronixSecretDecryptFail` | any | SEV2 | [Secret decryption failure](#secret-decryption-failure) |
| `VeyronixErrorBudget50` | 50% of any 30d budget consumed | — | [Error budget](#error-budget-exhaustion) |
