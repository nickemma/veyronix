# Plinth Runbook

This runbook defines the operating behavior of the current Plinth implementation and the drills that prove it. The fake backend is the dependency-free local path; the Kubernetes and Postgres paths use the same state and reconciliation boundaries.

## Operating principles

- Desired state is the source for what Plinth intends to run.
- Observed state is what the backend reports right now.
- Reconciliation is safe to repeat and is the recovery mechanism.
- Rollback selects a previous revision; it does not use a separate deployment path.
- The control plane is not in the workload request path.
- Never “fix” drift by editing generated Kubernetes resources manually. Change the manifest or the platform default, then reconcile.
- Keep failures visible: report the service, revision, failed condition, current revision, and the next useful command.

## Before declaring a phase complete

1. Run the unit and reconciliation tests.
2. Kill the reconciler during an action and restart it.
3. Introduce manual drift and confirm it is corrected.
4. Make the backend unavailable and confirm periodic retry behavior is observable and does not create duplicate revisions.
5. Follow the relevant procedure below without relying on undocumented knowledge.

## API documentation and playground

1. Start the local control plane against the fake backend.
2. Open Swagger UI and confirm the API operations match the current OpenAPI contract.
3. Open the playground and point it at the same local API.
4. Run the complete scenario in [`walkthrough.md`](walkthrough.md).
5. Confirm that actions made in the playground produce the same desired state, status, events, and audit information as the equivalent CLI commands.

The playground is for testing and explanation. Do not add dashboard features or make it a second source of operational truth.

## A service is not converging

1. Inspect `plinth status <service>`.
2. Identify the current revision and the first failing condition.
3. Inspect `plinth logs <service>` and the backend events.
4. Check whether the desired manifest is valid and whether the referenced image, configuration, and secrets exist.
5. If the current revision is unhealthy, keep the previous known-good revision active or run `plinth rollback <service>`.
6. If the failure is caused by a platform default, fix the reconciler or its configuration and let it converge again. Do not patch the generated resource as the permanent fix.

Expected feedback should be specific, for example:

```text
Deployment failed

tessera-gateway
Revision: 14

✗ Readiness probe failing
  GET /healthz → HTTP 503

Current revision: 13
Previous revision remains active.
Run `plinth logs tessera-gateway` for details.
```

## A pod or resource was deleted manually

1. Confirm the manifest still describes the resource.
2. Watch the service status.
3. Confirm reconciliation recreated the resource and that ownership metadata is correct.
4. Record the event as a drift-recovery test if this is a phase exercise.

The expected result is automatic correction because the loop is level-triggered.

## The reconciler was killed

1. Record which action was in progress.
2. Kill the process at that point.
3. Restart it against the same desired state and backend.
4. Confirm it observes reality and completes without duplicating resources or revisions.
5. Confirm the final status explains the recovered state.

This is the primary durability proof. A successful restart must not depend on an in-memory cursor.

## Kubernetes API is unavailable

1. Confirm the API failure in the control-plane health output.
2. Do not create a second desired revision merely because observation is delayed.
3. Allow the worker's periodic resync to retry without creating another desired revision.
4. Restore API access and confirm the queue/resync causes convergence.
5. Confirm already-running workloads continue serving throughout.

## Rollback

1. Run `plinth status <service>` and identify the last known-good revision.
2. Run `plinth rollback <service>` or select a specific prior revision.
3. Watch reconciliation and readiness until the previous revision is healthy.
4. Confirm the audit history records the actor, selected revision, previous revision, and outcome.
5. If rollback cannot restore health, pause/freeze further rollout and escalate with the service, revision, conditions, and logs.

## Progressive rollout abort

1. Confirm the rollout exceeded its error-rate policy.
2. Stop promotion of the new revision.
3. Reconcile the last known-good revision.
4. Verify health and error rate recover.
5. Preserve the failed revision and audit trail for diagnosis; do not delete evidence during recovery.

## Control-plane outage

The expected behavior is that workloads already applied to Kubernetes keep serving. Once Plinth returns, it must reload desired state and observed state, then resume reconciliation. This must be demonstrated before the final phase is complete.

## Operator and GitOps incidents

When the operator exists, use the CRD status conditions as the primary explanation and compare behavior with the standalone reconciler. When Argo CD is managing manifests, determine whether the desired-state change came from Plinth or Git before changing anything. Keep one clear owner for each resource to avoid two controllers fighting over it.

## Escalation record

For every unrecovered incident, capture:

- service and team;
- desired and observed revisions;
- first failing condition;
- relevant logs and backend events;
- whether the control plane or workload was affected;
- commands and changes made;
- final state and follow-up action.

If a procedure cannot be followed by a second engineer, update the runbook before calling the phase complete.
