# Config Reference

The public starter ships one canonical config template:

- `orchestrator/configs/orchestrator-config-v1.example.json`

## High-Value Sections

- `runtime`
  - rooted filesystem layout and control mode
- `remote`
  - public DNS/HTTPS settings, tokenized DoH settings, ACME inputs, and remote watchdog behavior
- `ssh`
  - listener settings and auth file locations
- `vpn`
  - Tailscale runtime and auth file location
- `trainBot`, `satiksmeBot`, `siteNotifier`
  - per-workload runtime roots, env files, release binaries, and public base URLs
- `ddns`
  - zone/record settings and token file path
- `modules`
  - enable or disable components by id

## Public Repo Rule

Use the example config as a template only.

- do not commit production hostnames
- do not commit real token paths unless they are generic placeholders
- keep environment-specific config in your private environment or private ops repo
