# Architecture Index

## Control Domains

- Android control plane: `orchestrator/android-orchestrator`
- Root runtime scripts and packaging: `orchestrator/scripts`
- Runtime templates and example config: `orchestrator/templates`, `orchestrator/configs`
- Managed workloads: `workloads/train-bot`, `workloads/satiksme-bot`, `workloads/site-notifications`
- External automation driver: `automation/task-executor`
- Shared contracts: `standards/schemas`, `orchestrator/modules`

## Key Contracts

- Module registry: `orchestrator/modules/registry/modules.yaml`
- Module manifest schema: `orchestrator/modules/schemas/module-manifest.v1.schema.json`
- Observability event schema: `standards/schemas/observability-event.v1.schema.json`
- Observability health schema: `standards/schemas/observability-health.v1.schema.json`

## Operational Shape

- The workstation builds artifacts and prepares deploy bundles.
- The rooted Pixel owns the runtime filesystem and lifecycle state.
- The Android app is the control plane for install, health, restart, and redeploy actions.
- Workloads remain separately owned modules even when they ship from the same monorepo.
