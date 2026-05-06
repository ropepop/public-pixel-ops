# Arbuzas Docker Layout

This directory is the active production deployment layout for the single-host Arbuzas runtime.

## What Lives Here

- `compose.yml`: the one active Docker Compose project for Portainer, apps, tunnels, DNS, and the ticket Android simulator.
- `env/arbuzas.example.env`: the operator template for hostnames, ports, and image pins.
- `images/`: Dockerfiles and entrypoints for the Arbuzas-owned workloads and DNS sidecars.

## Host Layout

- Persistent state: `/srv/arbuzas`
- Persistent ticket simulator AVD and store APK cache: `/srv/arbuzas/android-sim`
- Secrets and runtime env files: `/etc/arbuzas`
- Release bundles: `/etc/arbuzas/releases/<release-id>` with cleanup retaining current, rollback, and the newest 10 per release family
- Active release symlink: `/etc/arbuzas/current`

## Operator Entry Point

- Active deploy flow: `tools/arbuzas/deploy.sh`

Portainer runs directly against the local Docker socket on port `9443`. The live Arbuzas host must stay out of Docker Swarm, and the active repair flow now rewrites stale `tasks.agent` state in place before falling back to a clean first-run setup. The old Swarm and Pixel/orchestrator deployment paths are rollback-only legacy material.
The ticket Android simulator is Docker-private: no emulator, ADB, or simulator bridge port is published on the host, and no simulator tunnel exists.
The persistent simulator runs with 4 GB Android guest RAM inside a 6 GB total Docker memory cap and 2 cores, with Docker swap disabled. A Docker-private tuning loop turns Android zram/swap off after every emulator boot or restart.
When the local Pixel orchestrator debug APK is available, the ticket deploy uploads it to `/srv/arbuzas/android-sim/apks/pixel-orchestrator-debug.apk`, installs it in the persistent simulator, and starts the simulator ticket phone service. If the APK is unavailable locally, deploy reuses the remote cached copy if one already exists.
The ticket remote container includes ADB only for the owner-only simulator control surface, scoped to the Docker-internal `ticket_android_sim:5555` target.
The native Arbuzas DNS controlplane publishes encrypted DNS directly on host ports `443` and `853`.
