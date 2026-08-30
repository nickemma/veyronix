# Architecture Decision Records

Important implementation choices are recorded here as the project develops. Each ADR should state the context, the decision, the alternatives considered, and the consequences.

The canonical scope and phase order remain in [`../plinth.md`](../plinth.md). An ADR may explain how Plinth is built, but it must not silently expand what Plinth is.

The first ADRs should cover only decisions needed by the active phase, such as the manifest schema, desired-state revision model, and the boundary between the reconciler and its fake/Kubernetes backends.
