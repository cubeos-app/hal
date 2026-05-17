# 7. Parallel-dev workflow override

Date: 2026-05-18

## Status

Accepted

## Context

Per CubeOS Article XV the default is push-to-main + auto-deploy. Parallel-dev waves need MR-gated merges.

## Decision

Same as the CubeOS-family-wide ADR-0008: parallel-dev waves use `merge/<feature_id>` branch + 1 MR per feature + auto-delete on merge. Human work unchanged.

## Consequences

Same as the parent ADR; symmetry across all CubeOS family repos.
