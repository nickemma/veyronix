# Service Level Objectives and Error Budget Policy

| | |
|---|---|
| **Status** | Accepted — targets provisional until measured on real traffic |
| **Window** | Rolling 30 days |
| **Owner** | Platform |
| **Review** | Quarterly, or after any SEV1 |

---

## Why these exist

A platform every team depends on has to be measured like production, because it is production. "The deploy tool is a bit flaky" is not a statement anyone can act on. "Deployment success rate is 97.2% against a 99.0% objective, and the budget is 68% consumed with 11 days left in the window" is.

The targets below are deliberately not 100%. A 100% objective removes the ability to make trade-offs, and a system with no error budget cannot ship anything. The budget is a spending allowance, not a shameful residue.

**These numbers are provisional.** They were chosen from first principles before the platform served real traffic. They will be wrong. They get corrected against measured baselines after the first month of production use — see [Calibration](#calibration).

---

## Definitions

An **SLI** is a measurement. An **SLO** is a target for that measurement. The **error budget** is `1 − SLO` over the window, expressed in whatever unit makes it concrete.

Every SLI below specifies what is *excluded*, because an SLI that counts the user's own broken build against the platform is an SLI that gets ignored the first time it fires.

---

## The objectives

| # | SLI | Definition | SLO | Error budget (30d) |
|---|---|---|---|---|
| 1 | **API availability** | Non-5xx control plane responses ÷ total responses | 99.9% | ~43 min unavailable |
| 2 | **Deployment success rate** | `Succeeded ÷ (Succeeded + platform-caused Failed)` | 99.0% | 1 in 100 deploys |
| 3 | **Queue time** | Job persisted → worker claim, p95 | < 10s | 5% may exceed |
| 4 | **Mean time to deploy** | Deploy accepted → health check passed, p95 | < 5 min | 5% may exceed |
| 5 | **Rollback success rate** | Successful rollbacks ÷ attempted rollbacks | 99.5% | 1 in 200 |
| 6 | **Mean time to recovery** | Failed deploy → previous version healthy, p95 | < 3 min | 5% may exceed |
| 7 | **Event delivery lag** | Provider event emitted → visible in UI, p99 | < 2s | 1% may exceed |

### Exclusions, stated precisely

**SLI 2 excludes**, because they are not platform failures:
- User build errors (compile failure, failing tests, missing dependency)
- Health check failures caused by the user's application
- Provider-side outages — these are tracked separately as a provider availability SLI, since the platform cannot be more available than what it deploys to
- Deployments cancelled by a user

**SLI 2 includes**, because they are:
- Worker crashes that did not recover within the lease window
- Idempotency failures producing duplicate or missing releases
- Secret decryption failures caused by platform misconfiguration
- Queue starvation causing timeout
- Any deployment that reached a terminal state the state machine should not have permitted

**SLI 4 excludes** build time beyond a defined baseline. A user whose build takes 18 minutes is not experiencing a platform failure. The measured quantity is `queue + deploy + verify`, with build reported separately as an informational metric — which is why deployment duration is decomposed by phase rather than reported as a single number.

**SLI 5 is the most important number in this document.** Rollback is the control someone reaches for during *their* incident. Every other objective degrades gracefully; this one does not.

---

## Measurement

Recording rules over the Prometheus metrics defined in the [README](../../README.md#metrics):

```promql
# SLI 1 — API availability
sum(rate(veyronix_api_requests_total{code!~"5.."}[30d]))
  / sum(rate(veyronix_api_requests_total[30d]))

# SLI 2 — Deployment success rate (platform-caused failures only)
sum(rate(veyronix_deployment_total{outcome="succeeded"}[30d]))
  / sum(rate(veyronix_deployment_total{outcome=~"succeeded|failed_platform"}[30d]))

# SLI 3 — Queue time p95
histogram_quantile(0.95,
  sum by (le) (rate(veyronix_deployment_queue_wait_seconds_bucket[30d])))

# SLI 5 — Rollback success rate
sum(rate(veyronix_rollback_total{outcome="succeeded"}[30d]))
  / sum(rate(veyronix_rollback_total[30d]))

# Budget burn rate, SLI 2, multiplier against a 30-day budget
(1 - (
  sum(rate(veyronix_deployment_total{outcome="succeeded"}[1h]))
  / sum(rate(veyronix_deployment_total{outcome=~"succeeded|failed_platform"}[1h]))
)) / (1 - 0.99)
```

`outcome` must be labelled at the point of classification, in the deployment module's domain code, so that the "platform-caused or not" judgement lives in one place and is testable — not spread across dashboards where it can be quietly reinterpreted.

---

## Alerting on burn rate, not on threshold

Alerting the moment an SLI dips below its target produces noise and trains people to ignore alerts. Alert on **burn rate** — how fast the budget is being consumed relative to the window.

| Burn rate | Budget consumed in | Window | Severity | Action |
|---|---|---|---|---|
| 14.4× | 2 hours | 1h and 5m | Page | Something is actively broken |
| 6× | 5 hours | 6h and 30m | Page | Serious degradation |
| 3× | 10 days | 1d and 2h | Ticket | Investigate this week |
| 1× | Exactly the window | 3d and 6h | Ticket | Watch |

Each alert uses a long window and a short window together, so a burst that has already stopped does not page anyone.

---

## Error budget policy

This is the part with teeth. Without it, the SLOs are decoration.

| Budget consumed | Policy |
|---|---|
| **< 50%** | Normal operation. Ship features and providers freely. |
| **≥ 50%** | **New provider work stops.** Reliability work takes priority until the budget recovers. Existing provider work in flight finishes; nothing new starts. |
| **≥ 75%** | Above the previous, plus: every deployment to Veyronix itself requires a documented risk assessment. |
| **100%** | Deployments to the platform itself require review and sign-off. Only reliability fixes and rollbacks ship. |
| **Exhausted twice in a quarter** | The objective is wrong or the architecture is. Mandatory review of both. |

Two clarifications, because both get argued about:

1. **The policy applies per objective.** Exhausting the rollback budget stops feature work even if every other SLI is healthy. That is intentional — see SLI 5.
2. **Overriding the policy is allowed.** It is a decision with a name attached, recorded in the incident channel with a reason and an expiry. A policy that can never be overridden gets deleted; one that is overridden silently was never a policy.

---

## Calibration

The targets above are hypotheses. After the first 30 days of real traffic:

1. Measure actual performance per SLI, with no target in mind.
2. For each, ask: *would a user notice and care at this level?* An objective users cannot perceive is too strict; one they complain about is too loose.
3. Adjust the target, not the measurement. Moving the goalposts is legitimate; changing the definition to make the number look better is not.
4. Record the change and the reasoning in this document's history.

Expected corrections: SLI 3 (queue time) is probably too generous at 10s and can tighten; SLI 4 (time to deploy) is probably too aggressive at 5 minutes once real build times are included in the distribution; SLI 1 may be unachievable at 99.9% on a single-region deployment with a single Postgres, which is itself a useful thing to learn early.

---

## What is deliberately not an SLO

- **Dashboard load time.** Matters, not worth an error budget.
- **Build duration.** A user-owned property, tracked as a metric.
- **Provider API latency.** Not ours to control. Tracked for attribution, and used to exclude provider outages from SLI 2.
- **Notification delivery.** Best-effort by design; the event stream and audit log are the durable record.
