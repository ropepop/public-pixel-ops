# Public Pixel Ops

Public starter monorepo for a rooted Pixel operations stack.

This export mirrors the structure and deployment style of the private production repo, but removes operator-specific data, tracked secrets, evidence archives, and machine-local import tooling. It is meant to be a clean starting point for teams who want:

- an Android orchestrator app as the control plane
- rooted runtime scripts and templates under `/data/local/pixel-stack`
- workload modules with manifest-driven ownership
- scope-based redeploy workflows from a workstation over SSH or ADB
- lightweight validation and CI around contracts, docs, Go, Python, and shell tooling

## What This Repo Includes

- `orchestrator/`: Android orchestrator app, runtime scripts, templates, configs, module registry.
- `workloads/`: example managed services (`train-bot`, `satiksme-bot`, `site-notifications`).
- `automation/`: external scheduled automation (`task-executor`).
- `standards/`: shared manifest and observability templates.
- `tools/`: deploy helpers, contract checks, and docs validation.
- `infra/`: public-safe infrastructure manifests and placeholders.
- `docs/`: starter walkthroughs and reference material.

## What Was Removed

- tracked secrets and live credential files
- production config snapshots
- evidence archives and generated reports
- checked-in `.env` files
- private machine paths and import maps
- operator-specific hostnames, emails, and router defaults

## Quickstart

1. Read [docs/START.md](./docs/START.md).
2. Copy the relevant `.env.example` files and fill in your own values locally.
3. Review the orchestrator example config:
   - `orchestrator/configs/orchestrator-config-v1.example.json`
4. Run the validation checks that do not require private infrastructure:

```bash
./tools/import/validate_contracts.py
./tools/docs/check_links.sh
cd workloads/train-bot && go test ./...
cd ../site-notifications && PYTHONPATH=. pytest -q
cd ../../automation/task-executor && ./scripts/drain_runner_smoke_test.sh
```

## Deployment Model

The supported deploy path is:

1. Build artifacts on the workstation.
2. Package the orchestrator runtime bundle and any scoped workload release.
3. Connect to the rooted Pixel over SSH or ADB.
4. Run `tools/pixel/redeploy.sh` or a scoped workflow beneath it.
5. Let the Android orchestrator materialize config, place artifacts, and restart the affected components.

This repo keeps that contract intact while shipping only example defaults.

## Starter Walkthroughs

- [Start](./docs/START.md)
- [Manage](./docs/MANAGE.md)
- [Update](./docs/UPDATE.md)
- [Architecture](./docs/architecture/INDEX.md)
- [Orchestrator Reference](./docs/reference/orchestrator/README.md)

## Safety Notes

- Never commit `.env`, secret files, tunnel credentials, device dumps, or runtime evidence.
- Keep `ops/evidence/` and `ops/reports/` as generated-output areas only.
- Treat `orchestrator-config-v1.example.json` as the public template; create your own private environment-specific config outside version control.
