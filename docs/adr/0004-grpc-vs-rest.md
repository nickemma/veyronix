# ADR-0004 — Connect for the API surface

- **Status:** Accepted
- **Date:** 2026-07-22
- **Deciders:** @nickemma

## Context

Veyronix needs one API consumed by three very different clients: a Next.js dashboard in a browser, a Go CLI, and CI systems posting webhooks. Two of its most important operations — live deploy logs and deployment state events — are server-streaming, not request/response.

Plain REST handles the browser well and streaming badly (SSE works, but it is a second, hand-rolled protocol with its own reconnection and typing story). Plain gRPC handles streaming and internal calls well and browsers badly — it needs grpc-web plus a proxy, and the generated client story for TypeScript is heavier than it should be.

There is also a schema-duplication problem. Without a single source of truth, the same shape gets written three times: Go structs, TypeScript types, and Zod validators — and they drift.

## Decision

**Connect** (connectrpc.com), with one `.proto` per service under `api/proto/veyronix/v1/`.

- Connect speaks its own HTTP/1.1+JSON protocol, gRPC, and gRPC-Web from the same handler. The browser talks JSON over HTTP/1.1 with no proxy; internal service-to-service calls use gRPC.
- `buf` generates the Go server interfaces, the TypeScript client, and Zod schemas from the same protos, so the dashboard's runtime validation and the server's contract cannot drift.
- Server-streaming carries `StreamEvents` and `StreamLogs` — the concrete reason the transport is not plain REST.
- `api/openapi.yaml` is generated from the same protos for external consumers who want a familiar spec.

## Alternatives Considered

**Plain REST + JSON, with SSE for streaming.** Rejected: two protocols to maintain, hand-written types on both sides, and no schema source of truth. Familiar and browser-native, which is real value — but the drift cost compounds.

**Plain gRPC + grpc-web + Envoy.** Rejected: requires a proxy in the request path purely to serve a browser, adds a deployment component, and complicates local development for a solo maintainer.

**GraphQL.** Rejected: the API is a small set of commands and streams, not a graph traversal problem. Subscriptions would carry the streams, at the cost of a resolver layer and N+1 risk for no benefit here.

**tRPC.** Rejected: excellent inside a TypeScript monorepo, but the backend is Go and the CLI is Go. Non-starter.

## Consequences

**Positive**

- One schema; the Go server, TS client, Zod validators, and OpenAPI spec are all generated from it.
- No proxy between browser and API.
- Streaming is first-class rather than bolted on, and deadline propagation gives cancellation semantics for free — cancelling a deploy is a cancelled context all the way to the provider call.
- Errors are typed rather than stringly-typed HTTP statuses.

**Negative**

- `buf` becomes a hard build dependency; contributors must install it.
- Protobuf's JSON mapping is not quite what a hand-written REST API would produce (field naming, enum representation, `oneof` shape). Consumers occasionally find it surprising.
- Connect is a smaller ecosystem than plain REST. Fewer worked examples when something goes wrong.
- Schema evolution requires actual discipline — field numbers are forever.

## Revisit If

- A major consumer needs a REST-idiomatic API that the generated OpenAPI does not satisfy.
- Connect's maintenance situation changes materially.
