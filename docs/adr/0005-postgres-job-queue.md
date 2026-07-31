# ADR-0005 — Postgres-backed job queue with leases

- **Status:** Accepted
- **Date:** 2026-07-22
- **Deciders:** @nickemma

## Context

A deployment is a long-running job: clone, build, deploy, verify — minutes, not milliseconds. It must survive worker death, must not double-execute, must be cancellable, and must be observable while running.

The core question the whole system is judged on: *what is the state of a deployment when the machine running it disappears halfway through?* Most internal deploy scripts have no answer. Veyronix needs a specific one.

There is a hard constraint from [ADR-0003](0003-transactional-outbox.md): the job and its outbox event must commit atomically. That eliminates any job store outside the primary database.

## Decision

Jobs are **rows in Postgres** with an explicit state machine and **lease-based claiming**.

```sql
UPDATE jobs
   SET lease_owner = $1,
       lease_expires = now() + $2::interval,
       lease_version = lease_version + 1
 WHERE id = (
     SELECT id FROM jobs
      WHERE state = 'queued'
        AND (lease_expires IS NULL OR lease_expires < now())
        AND NOT EXISTS (
            SELECT 1 FROM jobs j2
             WHERE j2.project_id = jobs.project_id
               AND j2.environment = jobs.environment
               AND j2.state IN ('claimed','building','deploying','verifying'))
      ORDER BY priority DESC, created_at
      FOR UPDATE SKIP LOCKED
      LIMIT 1)
RETURNING *;
```

- `FOR UPDATE SKIP LOCKED` — two workers never contend for the same row.
- The `NOT EXISTS` clause — one in-flight deployment per `(project, environment)`, enforced in the database rather than in application logic, so it holds across arbitrarily many workers with no coordination.
- Lease TTL (`VEYRONIX_JOB_LEASE_TTL`, default 60s) with heartbeat every 15s. A dead worker's lease simply expires and the job is reclaimable — no reaper process, no external liveness system.
- `lease_version` — the heartbeat checks it still owns the lease. A worker that lost the race abandons the job instead of double-executing.

NATS carries the *notification* that work exists; Postgres remains the source of truth for what the work is and who holds it. Losing a NATS message costs latency, not correctness, because workers also poll.

## Alternatives Considered

**Redis (BullMQ / asynq / Sidekiq-style).** Rejected: cannot participate in the Postgres transaction that creates the deployment, which reintroduces the dual-write problem ADR-0003 exists to eliminate. Also adds a second stateful system whose durability guarantees are weaker than the one already in use.

**Temporal / Cadence.** The strongest alternative — durable execution, retries, timers, and history are exactly this problem, solved properly by people who specialize in it. Rejected for V1 on operational weight: Temporal is a substantial cluster to run, and adopting it means adopting its programming model throughout. Reconsider if the state machine grows saga-shaped branches (multi-provider deploys, coordinated multi-service releases).

**SQS / Cloud Tasks.** Rejected: ties a provider-agnostic platform to a specific cloud, cannot join the local transaction, and visibility timeouts are a weaker form of the lease already implemented.

**Kafka as the job store.** Rejected: partition-per-consumer semantics fit poorly with "one in-flight deploy per environment, arbitrary workers", and per-job state mutation is not what a log is for.

**In-memory goroutines with a supervisor.** Rejected: this is the failure mode the project exists to fix.

## Consequences

**Positive**

- Job creation and event publication are atomic.
- Worker death is a recoverable event with a bounded recovery window (lease TTL), not a stuck deployment.
- Job state is queryable with SQL — invaluable during an incident, and the reason `SELECT state, count(*) FROM jobs GROUP BY 1` is the first line of every runbook procedure.
- No additional stateful system to operate.

**Negative**

- Postgres is now on the hot path for job dispatch; queue depth and claim latency need monitoring (`veyronix_deployment_queue_wait_seconds`, `veyronix_deployment_queue_depth`).
- Polling has a floor on latency and a cost in wasted queries; NOTIFY reduces but does not remove it.
- Long-held locks or a table bloated with completed jobs will degrade claim latency. Completed jobs must be partitioned or archived; this is not optional past a few million rows.
- Lease TTL is a tuning knob with a real trade-off: too short and slow deploys get reclaimed mid-flight; too long and recovery from worker death is slow.

## Revisit If

- Claim latency p95 approaches the queue-time SLO under normal load.
- The workflow acquires genuine saga semantics — compensating transactions across multiple providers — at which point Temporal earns its weight.
