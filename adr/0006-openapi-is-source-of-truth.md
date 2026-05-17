# 6. OpenAPI spec is the source of truth for routes

Date: 2026-05-18 (codifying shipped design)

## Status

Accepted

## Context

HAL has ~150 endpoints across 22 functional domains. Multiple consumers (cubeos-api, MeshSat user app, monitoring scripts) integrate against the surface. Drift between the registered chi routes and the published OpenAPI spec causes:

- Operator-facing Swagger UI shows routes that 404 in reality.
- Client SDKs generated from OpenAPI miss real routes.
- New routes ship without operator visibility.

## Decision

`/docs/openapi.yaml` (served by `handlers/docs.go ServeOpenAPISpec`) is the canonical API documentation. Every PR touching `internal/handlers/routes.go` MUST include the matching OpenAPI update. CI MAY add a lint pass that diffs `chi.Walk(router)` against the parsed OpenAPI paths (parity check); deferred until a future spec.

Swagger UI is served at `/docs/` and `/docs/swagger-ui/*` so operators can explore live.

## Consequences

**Positive:**
- Single source of truth for "what does HAL expose?"
- Operator-facing docs match reality.
- Client SDKs generate cleanly.

**Negative:**
- PRs grow by ~10-30 lines of OpenAPI per new route. Acceptable cost.
- Manual sync until the parity CI lint lands. Easy to miss in a hurried PR — code review enforces.

**Enforced by:** Component Article C-VII + code review + future parity-lint CI gate.
