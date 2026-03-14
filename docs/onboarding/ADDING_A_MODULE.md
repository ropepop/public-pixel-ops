# Adding A Module

This starter uses a manifest-driven onboarding flow.

## Required Outputs

- a module directory in `workloads/`, `automation/`, `infra/`, or `orchestrator/`
- a `module.yaml` manifest that matches the schema
- a registry entry in `orchestrator/modules/registry/modules.yaml`
- a short README or doc entry that explains runtime ownership
- tests or validation coverage for the new component surface

## Redeploy Contract

Each managed component declares a `redeploy` block:

```yaml
redeploy:
  mode: artifact_release | asset_refresh | job | derived
  artifact_id: <artifact-id-when-applicable>
  derived_from: <owner-component-when-derived>
  release_root: <immutable-release-root-when-applicable>
```

Rules:

- `redeploy_component` is the default release path.
- `restart_component` controls lifecycle only and should not publish a new release.
- App-style services should own a dedicated runtime root.
- Derived components must name the component they depend on.

## Suggested Steps

1. Scaffold the module:

```bash
./tools/import/new_module_scaffold.sh <module_id> <domain_dir> <component_a,component_b>
```

2. Fill in the manifest and health checks.
3. Add the registry entry.
4. Add or update local docs.
5. Run:

```bash
./tools/import/validate_contracts.py
./tools/docs/check_links.sh
```
