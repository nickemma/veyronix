# Plinth End-to-End Walkthrough

This walkthrough is written for a second engineer who has not read the code.
It explains what Plinth does, starts it, exercises the lifecycle, and shows
how to verify the Kubernetes and operator paths.

## What was built, in plain language

Plinth is a small deployment platform. Instead of asking every developer to
write Kubernetes YAML, configure health checks, set security defaults, and
remember rollback commands, it gives them one service manifest and one
lifecycle API.

The important idea is reconciliation:

1. The developer declares what they want.
2. Plinth stores that desired state as a numbered revision.
3. Plinth checks what is actually running.
4. It repeatedly repairs the difference until reality matches the request.

This solves the common deployment problem where an imperative script stops
halfway through and leaves an unknown state. It also means a deleted workload
can be recreated, a process restart does not lose desired state, and a bad
revision can be restored through the same convergence loop.

From one manifest, the Kubernetes backend creates the service's Deployment,
Service, Ingress/TLS contract, ConfigMap, PodDisruptionBudget, and
default-deny NetworkPolicy. It also adds probes, resource limits, non-root
read-only security, Prometheus scrape annotations, structured-log metadata,
secret references, revision labels, and a DNS host. The operator path exposes
the same contract as a Kubernetes `PlinthService` custom resource.

## What this walkthrough proves

By the end, you will have verified:

- manifest validation and revision storage;
- convergence through the API, CLI, Swagger UI, and playground;
- resource observation and logs/events;
- manual drift repair;
- failed revision handling and rollback;
- progressive rollout safety;
- team membership, namespace selection, quotas, and audit records;
- recovery after stopping and restarting `plinthd`;
- Kubernetes golden-path resource creation;
- operator/CRD reconciliation and owner references;
- that running workloads do not depend on the control plane for requests.

## 1. Prerequisites

For the local path, install Go 1.22 or newer. For the Kubernetes path, also
install `kubectl`, a reachable Kubernetes cluster, and a kubeconfig. `kind`
is convenient for a disposable local cluster. For the operator path, the
cluster must allow CRDs and cluster-scoped RBAC.

From the repository root, build the binaries:

```bash
mkdir -p /tmp/plinth-bin
go build -o /tmp/plinth-bin/plinth ./cmd/plinth
go build -o /tmp/plinth-bin/plinthd ./cmd/plinthd
go build -o /tmp/plinth-bin/plinth-operator ./operator
```

Do not create a `.env` file. Plinth does not require one for local testing.

## 2. Start the local control plane

In terminal 1, start the fake backend with a disposable state file:

```bash
rm -f /tmp/plinth-walkthrough-state.json
/tmp/plinth-bin/plinthd \
  --backend=fake \
  --addr=:8080 \
  --state=/tmp/plinth-walkthrough-state.json
```

In terminal 2, check the process and open the browser surfaces:

```bash
curl -i http://localhost:8080/healthz
curl -i http://localhost:8080/readyz
```

Open `http://localhost:8080/docs` for Swagger UI and
`http://localhost:8080/playground` for the guided browser test client.

## 3. Apply and observe a service

Apply the known-good Tessera example through the CLI:

```bash
PLINTH_API=http://localhost:8080 \
  /tmp/plinth-bin/plinth up -f examples/tessera.plinth.yaml
```

The immediate response may be `pending` because the worker reconciles in the
background. Watch until it becomes `ready`:

```bash
PLINTH_API=http://localhost:8080 \
  /tmp/plinth-bin/plinth status tessera-gateway --watch
PLINTH_API=http://localhost:8080 \
  /tmp/plinth-bin/plinth logs tessera-gateway
```

The status shows desired, active, and known-good revisions plus the nine
golden-path resources. The same operation can be done in Swagger UI with
`POST /api/v1/services`, or in the playground with the example JSON.

Use the raw API to inspect the contract directly:

```bash
curl http://localhost:8080/api/v1/services/tessera-gateway | jq .
curl http://localhost:8080/api/v1/services/tessera-gateway/events | jq .
```

## 4. Introduce and repair drift

Ask Plinth's test helper to delete the fake Deployment and reconcile it:

```bash
curl -X POST http://localhost:8080/api/v1/services/tessera-gateway/test/drift \
  -H 'Content-Type: application/json' \
  -d '{"kind":"Deployment"}' | jq .
```

Expected result: the command reports the simulated deletion, then the
service returns to `ready`. The event and log history explain that the
resource was observed missing and applied again.

## 5. Test rollback and history

Submit the deliberately broken image as a second revision:

```bash
PLINTH_API=http://localhost:8080 \
  /tmp/plinth-bin/plinth up -f examples/tessera-broken.plinth.yaml || true
PLINTH_API=http://localhost:8080 \
  /tmp/plinth-bin/plinth status tessera-gateway --watch
```

The fake backend rejects the bad image. Because revision 1 was known good,
Plinth restores it and reports `rolled_back`. Confirm both revisions remain:

```bash
curl http://localhost:8080/api/v1/services/tessera-gateway | \
  jq '{phase,desired_revision,active_revision,last_known_good,history}'
```

You can explicitly select any historical revision:

```bash
PLINTH_API=http://localhost:8080 \
  /tmp/plinth-bin/plinth rollback tessera-gateway 1
```

## 6. Test progressive rollout safety

The error example enables stages 10%, 50%, and 100% with a five-percent
maximum error rate. The fake backend gives an image containing `error` a
50-percent error rate:

```bash
PLINTH_API=http://localhost:8080 \
  /tmp/plinth-bin/plinth up -f examples/tessera-error.plinth.yaml || true
PLINTH_API=http://localhost:8080 \
  /tmp/plinth-bin/plinth status tessera-gateway --watch
curl http://localhost:8080/api/v1/services/tessera-gateway/logs | jq .
```

Expected result: stage 10 is applied, the error-rate check aborts the
revision, and the previous known-good revision is restored. Kubernetes uses
the same logic but requires `--prometheus-url` and optionally
`--prometheus-query`; a missing or failing metric source fails closed.

## 7. Test teams, namespaces, quotas, and audit

Register a team as the global local actor:

```bash
curl -X POST http://localhost:8080/api/v1/teams \
  -H 'Content-Type: application/json' \
  -d '{"name":"payments","members":["alice"],"namespace":"plinth-payments","service_quota":20}' | jq .
```

Alice can deploy for that team:

```bash
PLINTH_API=http://localhost:8080 PLINTH_ACTOR=alice PLINTH_TEAM=payments \
  /tmp/plinth-bin/plinth up -f examples/lattice.plinth.yaml
```

An actor who is not a member is denied and the attempt is audited. For raw
JSON requests, send a JSON manifest body rather than the YAML file:

```bash
curl -i -X POST http://localhost:8080/api/v1/services \
  -H 'Content-Type: application/json' \
  -H 'X-Plinth-Actor: mallory' \
  -H 'X-Plinth-Team: payments' \
  --data '{"name":"unauthorized","image":"example:v1","port":8080,"replicas":1,"resources":{"cpu":"100m","memory":"128Mi"}}'
```

Inspect audit records in both formats:

```bash
curl http://localhost:8080/api/v1/audit | jq .
curl 'http://localhost:8080/api/v1/audit?format=csv'
```

## 8. Pause, resume, and destroy

These operations are lifecycle controls, not direct Kubernetes commands:

```bash
PLINTH_API=http://localhost:8080 /tmp/plinth-bin/plinth pause lattice
PLINTH_API=http://localhost:8080 /tmp/plinth-bin/plinth status lattice
PLINTH_API=http://localhost:8080 /tmp/plinth-bin/plinth resume lattice
PLINTH_API=http://localhost:8080 /tmp/plinth-bin/plinth destroy lattice
```

Pause leaves current resources serving. Destroy removes Plinth-managed
resources but keeps revision history.

## 9. Prove restart recovery

Stop terminal 1 with `Ctrl-C`, then start the same command again:

```bash
/tmp/plinth-bin/plinthd \
  --backend=fake \
  --addr=:8080 \
  --state=/tmp/plinth-walkthrough-state.json
```

The state file contains desired revisions and history. The worker queues the
stored services at startup and converges them again:

```bash
curl http://localhost:8080/api/v1/services | jq .
PLINTH_API=http://localhost:8080 /tmp/plinth-bin/plinth status --watch
```

## 10. Kubernetes backend and secrets

Use a disposable namespace. If it does not exist, `plinthd` creates and labels
it; the label permits platform ingress to the generated default-deny policy.

Create the externally managed Secret referenced by the Tessera manifest. The
value below is only a test placeholder:

```bash
kubectl create namespace plinth-test --dry-run=client -o yaml | kubectl apply -f -
kubectl -n plinth-test create secret generic tessera-gateway-secrets \
  --from-literal=DATABASE_URL='postgres://test-only@example.invalid/db' \
  --dry-run=client -o yaml | kubectl apply -f -
```

Start the Kubernetes control plane on another port if the fake process is
still running:

```bash
/tmp/plinth-bin/plinthd \
  --backend=kubernetes \
  --addr=:8081 \
  --state=/tmp/plinth-kubernetes-state.json \
  --namespace=plinth-test
```

Apply and wait:

```bash
PLINTH_API=http://localhost:8081 \
  /tmp/plinth-bin/plinth up -f examples/tessera.plinth.yaml
kubectl -n plinth-test rollout status deployment/tessera-gateway --timeout=180s
kubectl -n plinth-test get deployment,service,ingress,configmap,pdb,networkpolicy
```

Inspect the generated defaults:

```bash
kubectl -n plinth-test get deployment tessera-gateway -o yaml
kubectl -n plinth-test get ingress tessera-gateway -o yaml
kubectl -n plinth-test get networkpolicy tessera-gateway -o yaml
```

The Ingress requests `tessera-gateway.plinth.local` and a cert-manager TLS
Secret. A target cluster must provide an ingress controller, the
`plinth-default` cert-manager issuer, DNS, and an application endpoint before
the TLS URL can be tested. The generated Pod annotations are compatible with
Prometheus scrape discovery and structured-log collectors.

## 11. Operator and CRD path

The operator moves desired state into Kubernetes itself. Apply the CRD,
operator RBAC/deployment, and example custom resources:

```bash
kubectl apply -k operator
kubectl -n plinth-default get plinthservices
kubectl -n plinth-default get plinthservices -o yaml
kubectl -n plinth-default rollout status deployment/tessera-gateway --timeout=180s
```

Each `PlinthService` should gain a finalizer and `status.phase: Ready` after
its Deployment is available. Generated workload resources have an owner
reference to the CR, so deleting the CR lets the operator clean up its
resources before removing the finalizer.

If using the examples with a private or locally loaded image, edit the image
first and create the corresponding `<service>-secrets` Secret where needed.

## 12. GitOps with Argo CD

After Argo CD is installed and can read the GitHub repository, apply:

```bash
kubectl apply -f deploy/argocd/application.yaml
argocd app get plinth
argocd app get plinth-operator
```

The two Applications manage the platform deployment and operator
kustomization separately. Argo owns Git desired state; Plinth owns the
workload resources generated from `PlinthService` objects. Do not run two
controllers against the same generated resources.

## 13. Control-plane boundary

With a Kubernetes service already running, forward its Service directly:

```bash
kubectl -n plinth-test port-forward service/tessera-gateway 18087:8080
curl -i http://localhost:18087/healthz
```

Stop `plinthd` while the port-forward remains active and repeat the curl. The
workload continues serving because the control plane is not in the application
request path. Restart `plinthd` and confirm its persisted state and worker
reconcile again.

## 14. Cleanup

Remove only disposable resources created for this walkthrough:

```bash
kubectl delete namespace plinth-test --ignore-not-found
kubectl delete namespace plinth-payments --ignore-not-found
rm -f /tmp/plinth-walkthrough-state.json /tmp/plinth-kubernetes-state.json
```

If you used kind, delete only the named disposable cluster:

```bash
kind delete cluster --name plinth-e2e
```

## Completion checklist

Record the service name, revision numbers, phases, observed resources, and
timestamps from each step. A second engineer should be able to reproduce the
local path without private context and the Kubernetes path with only the
documented cluster prerequisites and workload images.
