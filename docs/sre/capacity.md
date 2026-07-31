# Capacity Planning and Load Test Results

| | |
|---|---|
| **Status** | Model only — **no load tests have been run yet** |
| **Last updated** | 2026-07-28 |
| **Owner** | Platform |

> Every number in §2 and §3 is a calculated estimate from the design, not a measurement. §4 is the plan for replacing them with real ones, and §5 is where results go. Nothing here should be cited as a benchmark until §5 has content.

---

## 1. What actually constrains this system

A deployment platform has an unusual load profile: request volume is trivial and per-request work is enormous. A thousand deploys a day is roughly one every 90 seconds — nothing for an API — but each one holds a worker slot for minutes and spawns a build container.

So the binding constraints, in order:

1. **Worker slots.** `replicas × VEYRONIX_WORKER_CONCURRENCY`. This is the real capacity number.
2. **Build resources.** CPU, memory, and disk for concurrent build containers — usually the actual ceiling, since builds are far heavier than deploys.
3. **Postgres connections and claim latency.** Every worker polls; every deploy writes.
4. **Per-environment serialization.** One in-flight deploy per `(project, environment)` by design. A team deploying the same environment repeatedly queues behind itself regardless of total capacity.

Constraint 4 means aggregate capacity can be ample while a specific team's experience is poor. Capacity planning here is about *distribution*, not just totals.

---

## 2. Sizing model

Assumptions, to be replaced by measurement:

| Parameter | Estimate | Basis |
|---|---|---|
| Deploy frequency | 3–10 per developer per day | Typical trunk-based team |
| Mean deploy duration | 4 min (build 2.5, deploy 1, verify 0.5) | Design targets |
| Peak-to-mean ratio | 4× | Deploys cluster before standup, before lunch, and at end of day |
| Build container | 2 vCPU, 4 GB, 10 GB scratch | Node/Go typical |
| Worker concurrency | 4 | Default |

Derived, for 50 developers at 6 deploys/day (300/day, ~12.5/h mean, ~50/h peak):

| Quantity | Estimate |
|---|---|
| Concurrent deploys at peak | 50/h × 4 min ÷ 60 = **~3.3**, call it 6 with jitter |
| Worker slots needed | 6 in flight + headroom → **12** (3 replicas × 4) |
| Build capacity at peak | 6 × 2 vCPU = **12 vCPU**, 24 GB |
| Postgres writes | ~5/deploy + heartbeats → **< 50 writes/s**, trivial |
| Events emitted | ~15/deploy → ~750/h peak, **trivial for JetStream** |
| Outbox rows | ~4.5k/day → partition weekly, retain 7 days |

The headline conclusion: **for organizations up to a few hundred developers, Postgres and NATS are nowhere near their limits, and build capacity is the thing to buy.** This is worth stating plainly because it is the opposite of where instinct sends people — the temptation is to worry about the queue, when the queue is the cheap part.

---

## 3. Estimated headroom by component

| Component | Estimated ceiling | Estimated headroom at the profile above | First symptom of saturation |
|---|---|---|---|
| Control plane API | ~2,000 RPS/instance | Very large | p95 latency rise, then 503 |
| Postgres (job claims) | ~500 claims/s with SKIP LOCKED | Very large | Claim latency rise → queue-time SLO burn |
| Postgres (storage) | Bounded by retention policy | Large with partitioning | Slow claims as the jobs table bloats |
| Outbox relay | ~5,000 rows/s single writer | Very large | Relay lag alert |
| NATS JetStream | ~100k msg/s | Very large | Consumer lag |
| **Worker slots** | `replicas × concurrency` | **~2× at peak** | **Queue time rises — this is the one that binds** |
| **Build resources** | Host CPU/memory/disk | **~2× at peak** | Build duration rises; container start failures |
| Provider APIs | Provider rate limits | Unknown, provider-specific | 429s, circuit breaker opens |

Provider rate limits are the least predictable entry and the one most likely to surprise: they are outside our control, differ per provider, and often apply per-account rather than per-target, so a large organization can hit them without any single project being unusual.

---

## 4. Load test plan

Not yet executed. Recorded here so the results in §5 are comparable across runs.

### Environment

Production-equivalent: same Postgres sizing, same worker container limits, NATS clustered as in production. A load test against a laptop measures the laptop.

### Scenarios

| # | Scenario | Purpose | Pass criterion |
|---|---|---|---|
| L1 | Steady state — 10 deploys/min for 1h, fake provider | Baseline throughput and latency | Queue time p95 < 10s; zero failures |
| L2 | Burst — 100 deploys submitted in 60s | Queue behaviour and fairness | All complete; no starvation of any project |
| L3 | Worker kill — SIGKILL a worker holding 4 jobs mid-build | Lease recovery | All 4 complete exactly once; recovery < 2× lease TTL |
| L4 | Postgres failover during in-flight deploys | Degradation behaviour | No lost jobs; in-flight resume post-failover |
| L5 | NATS outage for 5 min under load | Outbox durability | Zero lost events; backlog drains fully |
| L6 | Provider 429 storm | Circuit breaker and backoff | Other providers unaffected; jobs requeue, none fail permanently |
| L7 | Same-environment contention — 20 deploys to one environment | Serialization correctness | Exactly one in flight at all times; strict ordering |
| L8 | Sustained soak — 24h at 50% capacity | Leaks, table bloat, lease drift | Flat memory; claim latency flat; no bloat-driven regression |
| L9 | Table scale — 5M historical jobs, then run L1 | Bloat impact on claim latency | Claim latency within 20% of the empty-table baseline |

L3, L5, and L7 are the ones that matter most. They test the three properties the design actually claims: worker death is recoverable, broker loss is non-destructive, and per-environment serialization holds under load. If those three fail, the architecture is wrong rather than the sizing.

### Tooling

`k6` for API load; a purpose-built harness driving the deploy pipeline with `sdk/testing`'s fake provider so results measure the platform rather than Netlify's API. Chaos steps (L3–L6) scripted, not manual, so they are repeatable.

### Metrics captured per run

Queue wait p50/p95/p99; phase-decomposed deploy duration; claim latency; outbox lag; worker CPU/memory; Postgres connections, lock waits, and table size; error counts by class.

---

## 5. Results

_No runs recorded yet._

Each run appends a section here: date, commit SHA, environment, scenario, headline numbers, deviations from the model, and actions taken. Estimates in §2 and §3 get corrected against these rather than quietly left in place.

| Date | Commit | Scenario | Result | Notes |
|---|---|---|---|---|
| — | — | — | — | Awaiting first run |

---

## 6. Scaling playbook

| Symptom | Diagnosis | Action |
|---|---|---|
| Queue time p95 rising, workers busy | Worker starvation | Add worker replicas (prefer replicas over concurrency — build isolation is the constraint) |
| Build duration rising, queue flat | Build resource contention | Add build capacity, or split builds onto dedicated nodes |
| Claim latency rising, worker count fine | Jobs table bloat or lock contention | Archive completed jobs; verify the partial index on `(state, lease_expires)` |
| Outbox lag rising, NATS healthy | Relay throughput or outbox bloat | Prune published rows; consider CDC ([ADR-0003](../adr/0003-transactional-outbox.md)) |
| One team's deploys slow, platform healthy | Per-environment serialization | Expected. Split environments, or reduce deploy frequency to that environment |
| Provider 429s | Provider rate limit | Per-provider concurrency cap; request a limit increase |

---

## 7. Cost model (sketch)

At the 50-developer profile: 3 worker replicas (2 vCPU / 4 GB each), one Postgres instance (4 vCPU / 16 GB, 100 GB), one NATS node, one small API instance, plus observability. The dominant variable cost is build capacity, and it scales with deploy frequency rather than with headcount — which makes deploy frequency, not team size, the number to watch when forecasting.
