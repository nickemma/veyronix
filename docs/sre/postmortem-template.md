# Postmortem — <short incident title>

> **Copy this file to `docs/postmortems/YYYY-MM-DD-<slug>.md` and fill it in.**
>
> **This document is blameless.** Its subject is the system, not the person. Every human in this timeline acted reasonably given the information available to them at the time — if a decision looks wrong in hindsight, the question is what made it look right in the moment. Writing "X should have known better" means the analysis stopped one step too early.
>
> Delete these instruction blocks before publishing.

| | |
|---|---|
| **Incident ID** | INC-YYYY-NNN |
| **Date** | YYYY-MM-DD |
| **Severity** | SEV1 / SEV2 / SEV3 |
| **Duration** | Xh Ym (detection to resolution) |
| **Time to detect** | Xm (start to first alert or report) |
| **Time to mitigate** | Xm (detection to user-visible recovery) |
| **Author** | @handle |
| **Reviewers** | @handle, @handle |
| **Status** | Draft / In review / Published |

---

## Summary

Three to five sentences. What broke, who was affected, for how long, and how it was resolved. Someone should be able to read only this section and understand the incident.

> _Example shape:_ For 47 minutes, deployments to all Netlify targets failed at the verify stage. Twelve deployments across four teams were affected; six auto-rolled-back correctly and six were left on the previous version because the environment had rollback disabled. The cause was a health-check timeout that did not account for Netlify's CDN propagation delay. Resolved by raising the timeout and adding a propagation-aware readiness check.

---

## Impact

Be specific and quantitative. Vague impact statements produce vague action items.

- **Users affected:** how many, which teams, in what way
- **Deployments affected:** count, and their terminal states
- **Error budget consumed:** which SLI, what percentage of the 30-day budget
- **Downstream impact:** did anyone's own incident get worse because Veyronix was unavailable?
- **Data loss:** yes/no — if yes, exactly what

---

## Timeline

All times UTC. Include what people *knew* at each point, not just what they did — the gap between the two is usually where the real finding is.

| Time | Event |
|---|---|
| 09:14 | Change X deployed to production |
| 09:31 | First failing deployment (not yet noticed) |
| 09:38 | Alert `VeyronixX` fires |
| 09:41 | On-call acknowledges; initial hypothesis: provider outage |
| 09:52 | Netlify status page checked — no incident. Hypothesis discarded |
| 10:04 | Correlation found with the 09:14 change |
| 10:11 | Mitigation applied |
| 10:18 | Recovery confirmed |
| 10:25 | Incident closed |

---

## Detection

- How was it detected — alert, user report, or noticed by chance?
- How long between the first failure and detection?
- **If a user reported it before monitoring did, that is itself a finding** and needs an action item.

---

## Root cause

Not the first plausible explanation. Follow the chain until it reaches something structural.

Use "five whys" or a causal chain, and be honest about the last link:

```
Deployments failed at verify
  ← health check timed out after 30s
    ← Netlify CDN propagation exceeded 30s for large sites
      ← the timeout was chosen from small-site testing
        ← there was no test case for large sites
          ← the conformance suite has no size dimension
```

The finding is the last line, not the first. A postmortem that concludes "the timeout was too short" has produced one action item; one that concludes "the conformance suite does not vary input size" has produced a class of prevented incidents.

### Contributing factors

Conditions that made it worse or harder to resolve. These are not causes, and listing them separately keeps the root cause honest.

- e.g. rollback was disabled on six environments, so recovery was manual
- e.g. the runbook procedure assumed the provider status page was authoritative

---

## What went well

Not padding. Naming the controls that worked tells you what to protect during the next refactor.

- e.g. auto-rollback fired correctly on six of twelve environments
- e.g. blast radius was correctly confined to one provider
- e.g. the phase-decomposed metrics identified the verify stage in under a minute

---

## What went badly

- e.g. 24 minutes were spent on the provider-outage hypothesis before it was tested
- e.g. no alert covered per-provider verify-stage failures specifically

---

## Where we got lucky

The most valuable section in the document, and the one most often skipped. Luck is an unmitigated risk that has not been spent yet.

- e.g. it happened at 09:00 on a Tuesday, not during the Friday release window
- e.g. the KEK backup was never needed, and has still never been tested

---

## Action items

Every item has an **owner** (a person, not a team), a **due date**, and a **priority**. An item without an owner is a wish.

| # | Action | Type | Owner | Due | Priority | Issue |
|---|---|---|---|---|---|---|
| 1 | | Prevent / Detect / Mitigate / Process | @ | YYYY-MM-DD | P0–P2 | # |

Guidance:

- Prefer **prevent** over **detect**, and **detect** over **mitigate**. A faster response to a recurring problem is the weakest outcome.
- "Add more monitoring" is not an action item. "Alert on verify-stage failure rate per provider above 5% for 5 minutes" is.
- "Be more careful" is never an action item.
- If an action item is "update the runbook", write the update as part of the postmortem rather than promising it.

---

## Supporting material

- Dashboards, log queries, traces (with time ranges pinned)
- Relevant ADRs — and whether the incident invalidates any of them
- Related incidents — if this is the second occurrence, say so prominently and treat recurrence as its own finding

---

## Review

| | |
|---|---|
| **Reviewed on** | YYYY-MM-DD |
| **Attendees** | |
| **Action items agreed** | N |
| **ADRs to revisit** | |
