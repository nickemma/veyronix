# Standalone Control Plane versus Kubernetes Operator

This comparison is the Phase 6 artifact for the two implementations in this repository. It records the behavior and trade-offs visible in the code and tests; live cluster timings can be added when a cluster is available.

## Short version

The standalone control plane teaches the control-loop mechanics with an explicit service API, revision store, CLI, and backend boundary. The operator places reconciliation inside Kubernetes and uses a CRD as the user-facing desired-state contract.

Neither is universally better. The right choice depends on where the desired state belongs, how tightly the product is coupled to Kubernetes, and who should operate it.

## Comparison frame

| Concern | Standalone Plinth control plane | Kubernetes operator |
|---|---|---|
| Desired-state entry point | `plinth.yaml`, CLI, and service API | A Kubernetes custom resource |
| State storage | Plinth's revision store plus observed backend state | Kubernetes API, CRD status, and controller-owned resources |
| Kubernetes coupling | Adapter boundary allows a fake backend and a separate API surface | Directly coupled to Kubernetes API and controller conventions |
| User experience | Can present a focused golden path and lifecycle commands | Fits naturally into `kubectl`, GitOps, and Kubernetes workflows |
| Recovery model | Plinth restarts and reconciles from stored desired plus observed state | Controller restarts and reconciles from the API server's durable objects |
| Operational surface | A service, database, and backend integration to run | Controller deployment, CRDs, RBAC, and Kubernetes lifecycle to operate |
| Testing | Fast fake-backend tests plus adapter tests | Reconcile tests, API-server tests, and cluster-level tests |
| Best fit | A platform with a focused product contract or multiple front doors | Kubernetes-native teams that want the API server to own the contract |

## Questions answered by this comparison

1. What behavior was simpler to express in the standalone control plane?
2. What behavior became simpler once the CRD and status conditions existed?
3. Which implementation made drift, retries, and ownership easier to reason about?
4. What did the operator approach give up in API design, portability, or user experience?
5. At what adoption or complexity threshold is the CRD worth introducing?
6. How do GitOps and Argo CD change the answer?

## Conclusion

The standalone version is the teaching and product-design instrument: it makes the control loop, revision history, lifecycle, and golden path explicit. The operator is the Kubernetes-native packaging of the same idea and is the better fit when Kubernetes is already the platform API. The standalone service contract is easier to expose to non-Kubernetes clients; the operator gets durable desired state, native watches, owner references, status conditions, garbage collection, and GitOps integration from Kubernetes itself.
