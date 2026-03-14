# Filesystem And State

## Rooted Runtime

- `/data/local/pixel-stack/bin`
  - component entrypoints
- `/data/local/pixel-stack/conf`
  - orchestrator config, env files, and secret file mount points
- `/data/local/pixel-stack/run`
  - pid files and transient state
- `/data/local/pixel-stack/logs`
  - runtime logs and event output
- `/data/local/pixel-stack/apps`
  - workload runtimes and release directories

## Workload Roots

- `/data/local/pixel-stack/apps/train-bot`
- `/data/local/pixel-stack/apps/satiksme-bot`
- `/data/local/pixel-stack/apps/site-notifications`

Each app-style workload should keep:

- a stable runtime root
- immutable releases when applicable
- a `current` symlink or equivalent active pointer
- separate `env`, `run`, `logs`, and `state` areas

## Repo-Local Generated Output

These directories are intentionally runtime-generated and should stay untracked:

- `ops/evidence/`
- `ops/reports/`
- `output/`
- `state/`
