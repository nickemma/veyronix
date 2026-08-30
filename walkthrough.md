# Plinth End-to-End Walkthrough

This is the verification path for Plinth. It is written as the final user journey, then becomes executable as each phase is implemented. The CLI, Swagger UI, and playground must exercise the same control-plane behavior.

## What this proves

By the end of this walkthrough, we should be able to show:

- a manifest being validated and accepted;
- desired state being stored as a revision;
- the reconciler creating and converging resources;
- status, events, and logs being visible through the CLI and HTTP surfaces;
- manual drift being corrected;
- a bad revision being detected and rolled back;
- recovery after the reconciler is killed;
- the control plane being unavailable without taking running workloads down.

## Prerequisites

For the local path, use the in-memory fake backend. For cluster verification, use a disposable Kubernetes namespace or test cluster with `client-go` access.

The final walkthrough assumes:

```text
plinth       CLI
plinthd      control plane and reconciler
Swagger UI   http://localhost:8080/docs
Playground   http://localhost:8080/playground
API          http://localhost:8080/api
```

The exact ports and flags may change; the links should remain discoverable from the startup output and README.

## 1. Start Plinth

Start the control plane with the fake backend first:

```bash
plinthd --backend=fake
```

Confirm the process reports that it is ready and that the fake backend starts empty. In the Kubernetes phase, repeat this with an isolated namespace and the Kubernetes backend.

Open both browser surfaces:

```text
http://localhost:8080/docs
http://localhost:8080/playground
```

Swagger UI is the API reference. The playground is the guided test client.

## 2. Submit a service manifest

Create `examples/tessera.plinth.yaml` or use the example manifest in the playground:

```yaml
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

Submit it in the playground, through the Swagger UI request form, and with the CLI:

```bash
plinth up -f examples/tessera.plinth.yaml
```

Expected result:

- invalid fields are rejected before reconciliation;
- a valid manifest creates a new revision;
- the response identifies the service and revision;
- the desired state is visible through the API and CLI.

## 3. Watch convergence

Use all three views to inspect the same operation:

```bash
plinth status tessera-gateway --watch
plinth logs tessera-gateway --follow
```

In Swagger UI, call the status and events operations. In the playground, watch the status timeline. The service should move from pending to converged, and the output should explain the observed resources rather than only saying “deployment failed.”

With the Kubernetes backend, confirm the generated resources include the golden-path defaults: Service, Ingress, TLS/DNS integration, probes, metrics, logs, resource limits, security context, PodDisruptionBudget, and NetworkPolicy:

```bash
plinthd --backend=kubernetes --namespace=plinth-test
```

Use a disposable namespace and the kubeconfig selected by your environment. The same CLI, Swagger UI, and playground requests should work against either backend.

## 4. Introduce drift

Delete a managed resource manually in the test environment. For example, delete one pod or the Deployment using the appropriate test-cluster command.

Then observe:

```bash
plinth status tessera-gateway --watch
```

Expected result: the watch or resync notices the difference, the reconciler restores the resource, and the event history explains that it corrected drift. The developer should not need to submit the manifest again.

## 5. Test rollback

Submit a second revision with a deliberately bad image or a failing readiness endpoint. Observe the failure in the CLI, Swagger UI, and playground.

```bash
plinth status tessera-gateway --watch
plinth logs tessera-gateway --follow
```

Then roll back:

```bash
plinth rollback tessera-gateway
```

Expected result:

- the failed revision remains in history;
- the previous known-good revision becomes desired state;
- reconciliation restores it;
- the final status identifies the failed condition and restored revision;
- the audit record contains the actor, revisions, timestamps, and outcome.

## 6. Kill and restart the reconciler

Start a reconciliation action, then terminate `plinthd` while it is in progress. Restart it with the same desired-state store and backend.

```bash
plinthd --backend=fake
```

Expected result: Plinth reloads desired and observed state, continues from reality, and converges without duplicating resources or revisions. This is the central proof that the system is a control plane rather than a fragile imperative script.

## 7. Test the control-plane boundary

With a service already running, stop Plinth. Continue sending requests to the service directly and confirm it remains available. Start Plinth again and confirm reconciliation resumes.

This proves the control plane is not in the application request path.

## 8. Repeat through the playground

The playground should provide a guided version of the same sequence:

1. load or edit a manifest;
2. validate it;
3. apply it;
4. watch status and events;
5. view logs;
6. introduce a test failure or drift event;
7. roll back;
8. inspect the final revision history.

It should show the API request and response for each action so the user can move from clicking a test control to understanding the underlying contract.

## Completion evidence

Save screenshots or terminal output for the successful converge, drift repair, rollback, and restart recovery. Record the commands, revision numbers, observed conditions, and timing. The walkthrough is complete only when another engineer can reproduce it from this file without relying on private context.
