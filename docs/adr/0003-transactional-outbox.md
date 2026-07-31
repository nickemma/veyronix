# ADR-0003 — Transactional outbox over dual write

- **Status:** Accepted
- **Date:** 2026-07-21
- **Deciders:** @nickemma

## Context

Creating a deployment must do two things: persist the job, and tell a worker about it. These live in different systems — Postgres and NATS — and there is no distributed transaction between them.

The naive implementation writes both in sequence:

```go
tx.Insert(job)      // commits
tx.Commit()
bus.Publish(event)  // may fail
```

If the publish fails, or the process dies between the two, a job exists that no worker will ever hear about. The user sees "Queued" forever. Reversing the order is worse: an event fires for a job whose transaction then rolls back, and a worker executes a deployment that does not exist.

Neither failure is loud. Both are rare enough to survive testing and common enough to happen in production. This is the defining correctness problem at the DB/broker boundary.

## Decision

The **transactional outbox**. The job row and an `outbox` row commit in a single Postgres transaction:

```sql
BEGIN;
  INSERT INTO deployments (...) VALUES (...);
  INSERT INTO jobs        (...) VALUES (...);
  INSERT INTO outbox (topic, payload, created_at)
       VALUES ('deployment.created', $1, now());
  INSERT INTO audit_log (...) VALUES (...);
COMMIT;
```

A separate relay process (`veyronix-relay`) polls unpublished outbox rows in commit order, publishes to NATS, and marks them published. Polling interval is `VEYRONIX_OUTBOX_POLL_INTERVAL` (default 1s), with `LISTEN/NOTIFY` as a latency optimization on top — the poll remains the correctness mechanism, so a missed notification costs latency, not delivery.

If the relay dies after publishing but before marking, the row republishes. Delivery is therefore **at-least-once**, and every consumer must be idempotent. They are: events carry the deployment ID and a per-deployment monotonic sequence number, so duplicates are detectable and out-of-order arrival is orderable.

## Alternatives Considered

**Dual write with retry.** Rejected: retries do not help when the process dies between the two operations, which is precisely the case that produces a stuck deployment. It converts a common failure into a rare one and calls it solved.

**Change data capture (Debezium on the WAL).** Correct, and strictly better on latency and load. Rejected for V1: it adds Kafka Connect or an equivalent runtime, WAL configuration, and replication slot operations to a system with one maintainer. Legitimate at higher scale.

**Listen/notify alone.** Rejected as the correctness mechanism: notifications are not durable. A consumer disconnected at the moment of `NOTIFY` never learns of it. Kept as a latency optimization above the poll.

**Publish from the worker rather than the API.** Rejected: it moves the same dual-write problem one hop later without solving it.

**No broker; workers poll the jobs table directly.** Rejected. Genuinely simpler for job dispatch — and honestly viable at V1 scale — but the event stream feeds the UI, notifications, and metrics as well as workers. A broker is the right shape for fan-out to consumers with different durability needs.

## Consequences

**Positive**

- No lost deployments, no phantom deployments — the two are atomic by construction.
- The outbox is a durable, ordered audit of everything the system intended to publish.
- If NATS is down, jobs accumulate in Postgres and drain on recovery. Broker unavailability degrades latency, not correctness.

**Negative**

- Publication latency is bounded below by the poll interval (mitigated by NOTIFY).
- The outbox table grows and needs pruning — a partitioned table with a retention job, not `DELETE` in a loop.
- Every consumer must be idempotent. This is a permanent obligation on all future consumers, and it is easy to forget when adding the fifth one.
- One more process to run and monitor. Relay lag is an alerting SLI, not an afterthought.

## Revisit If

- Outbox relay lag becomes a recurring SLO burn source → move to CDC.
- Event volume outgrows single-writer Postgres throughput.
