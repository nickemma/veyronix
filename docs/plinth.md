# PLINTH

**A minimal internal developer platform. The Veyronix replacement — smaller, personal, and finishable.**

`github.com/nickemma/veyronix` · Project 3 · weeks 27–34

*A plinth is the base a structure stands on. Rename it if something better lands — the name isn't load-bearing, the control loop is.*

---

## Why this exists

Veyronix was going to be your platform-engineering evidence and it's off the table. Platform engineering is a role family — Developer Platform, DevEx, Internal Tools, Kubernetes Platform — that you'd otherwise have no artifact for, and it's one of the highest-demand infrastructure specialisations there is.

Plinth is the minimal replacement. Eight weeks, not eight months. Small enough to finish, real enough that its core loop is the same one Kubernetes itself runs on.

**What it is not:** a Veyronix clone. Veyronix was provider-agnostic, multi-module, enterprise-shaped. Plinth does one thing.

---

## What we're trying to achieve

Right now, deploying a service means: write a Dockerfile, write Kubernetes manifests, wire up TLS, add Prometheus annotations, configure log shipping, mount secrets, set resource limits, add probes, configure an ingress. Every team does it slightly differently, everyone gets something wrong, and nobody remembers how to roll back at 2am.

Plinth replaces all of that with one file and one command.

```yaml
# plinth.yaml
name: tessera-gateway
image: ghcr.io/nickemma/tessera:v0.4.1
port: 8080
replicas: 3
env:
  LOG_LEVEL: info
secrets:
  - DATABASE_URL
resources:
  cpu: 500m
  memory: 512Mi
```

```bash
plinth up
```

And the service is running, on a URL, with TLS, metrics being scraped, logs being shipped, secrets injected, resource limits enforced, running as non-root, with health probes wired and `plinth rollback` available.

**That's a golden path.** The developer never touches a Kubernetes manifest. Everything correct-by-default happens because the platform did it, not because they remembered. Secret values stay outside the application database; the manifest declares references that the target cluster's secret mechanism supplies.

**The claim it earns you:** *"I built a control plane with a reconciliation loop. It's the same pattern Kubernetes runs on, and I can explain why reconciliation beats imperative deployment when things go wrong."*

---

## The one idea this project exists to teach

**Reconciliation.**

Almost every deployment tool people write is imperative: "do these steps in order." That works until step 4 fails, and now you're in a state that's neither the old thing nor the new thing, and nobody knows how to get out.

Control planes work differently. They store **desired state**, they continuously observe **actual state**, and they run a loop that makes the second look like the first. If something fails, the loop just runs again. If someone deletes a pod by hand, the loop puts it back. If the control plane itself restarts, the loop picks up where reality is, not where a script thought it was.

That single idea is what makes Kubernetes work, what makes Argo CD work, what makes Terraform's plan/apply work, and what makes an operator an operator. Building one yourself is the difference between using Kubernetes and understanding it.

---

## What it teaches — and when

### Phase 1 · Week 27 — Declarative versus imperative

The concept, properly, before any code. Desired state. Observed state. Convergence. Idempotency — why every action must be safe to repeat. Drift. Level-triggered versus edge-triggered logic and why edge-triggered systems lose events and level-triggered ones don't.

**Built:** a 100-line reconciler over a fake backend — a map in memory. Desired state says three replicas, actual says one, the loop creates two. Delete one by hand, the loop notices and restores it. No Kubernetes involved yet.

**Broken:** kill the reconciler mid-action and restart it. Prove it converges anyway. That's the property that matters, and proving it on a toy is how you understand it on a real one.

### Phase 2 · Week 28 — The manifest, API, CLI, and test surfaces

Schema design and validation — reject bad input at the edge with a useful message, never halfway through a deploy. The CLI: `plinth up`, `status`, `logs`, `rollback`, `pause`, `destroy`. Server-side storage of desired state in Postgres, with revision history — because rollback is just "make revision N-1 the desired state again and let the loop do the rest."

The control plane also exposes the lifecycle through a documented HTTP API. Swagger UI documents the API contract, and a small browser playground exercises the same API for local and test-cluster verification. These are documentation and testing surfaces, not a product dashboard.

### Phase 3 · Weeks 29–30 — The Kubernetes API from the inside

Not `kubectl`. The API. `client-go`: typed clients, informers, work queues, watch semantics, resync, optimistic concurrency and resource versions, owner references and cascading deletion, server-side apply.

**Built:** the Kubernetes adapter. Your reconciler now creates real Deployments, Services, Ingresses, ConfigMaps, PodDisruptionBudgets, and NetworkPolicies, with secret references wired into the workload contract.

**Broken:** two reconcilers running at once — watch the conflict, then fix it with resource versions. Delete a Deployment out from under the control plane. Make the API server unreachable mid-reconcile.

### Phase 4 · Week 31 — The golden path

The part that makes it a platform rather than a deploy script. Every service that goes through Plinth automatically gets: TLS via cert-manager, a DNS name, Prometheus scrape configuration, structured log shipping, liveness and readiness probes, resource requests and limits, a non-root security context with a read-only root filesystem, a pod disruption budget, and a default-deny network policy.

The developer asked for none of it. That's the point. **A platform is opinionated defaults that are hard to get wrong**, not a menu of options.

### Phase 5 · Week 32 — Tenancy and safety

Teams, namespaces, RBAC. Quotas per team. An audit log of every change: who deployed what, when, and what the previous revision was. Then the safety features that make it usable by people who aren't you: `plinth rollback` to any previous revision, and a progressive rollout that watches error rate and aborts itself.

### Phase 6 · Week 33 — Become an operator, and compare

Rebuild the core as a genuine Kubernetes operator: a CRD, `kubebuilder`, a controller with a reconcile function, status subresources and conditions, finalizers.

Then write the comparison. Your standalone control plane versus the operator: what each is better at, when the CRD approach is worth it, what you gave up. **That comparison document is the most interview-valuable artifact in this project.** Very few candidates can argue both sides of it from experience.

### Phase 7 · Week 34 — GitOps, and eat your own cooking

Argo CD, and the manifests-in-git model. Then the real test: **deploy Tessera and Lattice with Plinth.** If your own platform can't run your own systems, it isn't a platform.

**Broken:** the control plane goes down while services are running — do they keep serving? (They must. The control plane is not in the request path, and knowing that is a design principle you'll be asked about.) Then: a bad image tag, a service that never becomes ready, a secret that doesn't exist, a rollout that has to abort itself.

---

## Done means

- [ ] `plinth up` takes a manifest and produces a running, reachable, TLS-terminated service (live cluster proof)
- [x] The reconciler converges from any starting state, including after being killed mid-action (fake and client tests)
- [x] Manual drift is detected and corrected — delete a pod, watch it come back for the right reason (fake path and test helper)
- [x] Rollback to any previous revision, demonstrated and timed (fake path)
- [x] The golden path applies all nine defaults without the developer asking (fake and client-go adapters)
- [x] Multi-team with RBAC policy, quotas, and an audit log
- [x] The operator/CRD version exists, and the comparison document is written
- [x] **Tessera and Lattice are deployed by Plinth** (disposable kind-cluster proof)
- [x] The control plane can be down while workloads keep serving — proven, not assumed (direct workload check)
- [x] Swagger UI documents the API and the playground can exercise it end to end
- [x] `README.md` · `DESIGN_DOC.md` · `ADR/` · `RUNBOOK.md` · `walkthrough.md` · the comparison doc

The checked items are implemented and covered by local, fake-client, or disposable-cluster evidence. The remaining TLS item requires a target cluster with an ingress controller, cert-manager issuer, and DNS routing; the repository includes the manifests and exact commands for that final proof.

---

## Roles this opens

Developer Platform Engineer · DevEx Engineer · Internal Tools Engineer · Kubernetes Platform Engineer · Infrastructure Engineer · SRE (platform-leaning) · Solutions/Platform Architect.

It also strengthens every application already open, because "I built a control plane" is a much stronger signal than "I used Kubernetes."

---

## Repo layout

```
plinth/
├─ cmd/{plinth,plinthd}/          CLI and control plane
├─ internal/
│   ├─ api/              HTTP API, Swagger UI, playground
│   ├─ backend/          fake and Kubernetes adapters
│   ├─ manifest/         parse and validate
│   ├─ reconcile/        the loop and worker
│   ├─ state/            file-backed and Postgres persistence
│   └─ tenancy/          teams and quotas
├─ operator/                      the CRD version
├─ examples/                      tessera.plinth.yaml, lattice.plinth.yaml
├─ deploy/  └─ docs/
│   └─ walkthrough.md    end-to-end verification
```

Keeping `backend.Fake` in the finished product is deliberate — it makes the reconciler testable without a cluster and demonstrates that the backend boundary is a real design decision rather than folder decoration.

---

## Scope discipline

Eight weeks means saying no. **Not building:** a product dashboard, multi-cloud support, service mesh integration, cost management, a plugin system, or anything Veyronix had that isn't the control loop. Swagger UI and the test playground are deliberately narrow exceptions: they make the API and end-to-end behavior observable without becoming a second product.

If it's week 33 and you're tempted to add a dashboard, the answer is no. Write it in `DEBT.md` and ship on time. A finished small platform beats an unfinished large one, and you have direct experience of which one you tend to build.


### What we'll add
There are a few IDP capabilities that would make the platform feel more complete without blowing up your eight-week scope.

- 1. Environment promotion
Right now we have deployment and rollback, but let's consider explicitly demonstrating:
dev → staging → production
with the same service contract moving between environments.

- 2. Configuration management
We have secrets, but let's distinguish:
application config
      ≠
secrets
      ≠
platform configuration

Example:
name: tessera-gateway
image: ...
port: 8080

config:
  LOG_LEVEL: info

secrets:
  - DATABASE_URL
Plinth decides how those become Kubernetes resources/environment variables/etc.
  
- 3. Service lifecycle
We'd add explicit commands/concepts for:
plinth up
plinth status
plinth logs
plinth rollback
plinth pause
plinth destroy
Not because you need a huge CLI, but because self-service includes the lifecycle, not just deployment.

- 4. Platform feedback
A real IDP needs to tell the developer why something failed.

Instead of:
   - Deployment failed
Plinth should surface something like:
   Deployment failed
   
   tessera-gateway
   Revision: 14
   
   ✗ Readiness probe failing
     GET /healthz → HTTP 503
   
   Current revision: 13
   Previous revision remains active.
   Run `plinth logs tessera-gateway` for details.
 
 
 That reinforces your philosophy of making the correct thing easy and the incorrect thing difficult.
