# ADR-0006 — NATS JetStream as the broker

- **Status:** Accepted
- **Date:** 2026-07-23
- **Deciders:** @nickemma

## Context

The outbox relay needs somewhere to publish, and several consumers with different needs read from it: workers (must not miss a job), the dashboard event stream (needs low latency, tolerates gaps by re-reading from Postgres), notifications (at-least-once, deduplicated), and the metrics pipeline.

Requirements: durable streams, at-least-once delivery, consumer groups, replay from a sequence number, and — for a solo maintainer — operational weight proportional to the value delivered.

## Decision

**NATS JetStream.**

- One stream per subject family: `deployment.*`, `project.*`, `audit.*`.
- Durable pull consumers for workers (explicit ack, redelivery on nak or timeout).
- Ephemeral push consumers for dashboard streaming — a browser session does not need durable state, and the UI can resume from the last `seq` it saw via the API.
- Retention by age and size, not forever. Postgres is the system of record; the stream is transport.

## Alternatives Considered

**Kafka (or Redpanda).** Rejected on operational weight. Kafka is the right answer at a scale Veyronix is nowhere near, and the wrong answer for one maintainer running a platform in early design. Redpanda removes the ZooKeeper/KRaft complexity but not the mental model. Revisit at sustained high-thousands of events per second or when log compaction and long retention become genuinely load-bearing.

**Redis Streams.** Rejected: consumer group semantics are workable, but durability depends on persistence configuration that is easy to get subtly wrong, and this system's whole thesis is not losing deployments.

**Postgres LISTEN/NOTIFY as the only transport.** Rejected as the primary mechanism: notifications are not durable and there is no replay. It stays as a latency optimization above the outbox poll.

**RabbitMQ.** Reasonable and mature. Rejected: routing flexibility is not the constraint here, and replay from a sequence number — needed by the dashboard on reconnect — is not its natural model.

**No broker at all; workers poll the jobs table.** Genuinely viable at V1 scale and rejected only because the event stream fans out to consumers beyond workers. If fan-out ever collapses back to workers alone, this becomes the right answer again and the broker should be removed rather than kept for appearances.

## Consequences

**Positive**

- Single Go binary, small footprint, runs locally in docker-compose without ceremony.
- Durable streams with replay, which the dashboard reconnect path depends on.
- Consumer groups give worker scale-out with no additional coordination.

**Negative**

- Smaller ecosystem than Kafka; fewer off-the-shelf connectors and fewer answers when something misbehaves.
- JetStream's configuration surface (retention, ack policy, max deliver, backoff) is easy to set wrongly in ways that only appear under failure. These settings belong in code review, not in someone's shell history.
- Another stateful component to back up and monitor — though its loss is recoverable from the outbox, which is the point.

## Revisit If

- Event throughput or retention requirements outgrow JetStream.
- The system needs log compaction or multi-consumer replay measured in weeks.
