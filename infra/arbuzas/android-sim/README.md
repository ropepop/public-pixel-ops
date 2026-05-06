# Arbuzas Android Simulator Lab

This directory is an experimental, private-only Android simulator lab for Arbuzas.

It is intentionally separate from the production Compose stack in `infra/arbuzas/docker/compose.yml`.
The simulator must not be added to the normal `tools/arbuzas/deploy.sh` service inventory unless there
is a later explicit decision to promote it.

## Fixed Trial Shape

- Android image: `halimqarroum/docker-android:api-33` or `halimqarroum/docker-android:api-33-playstore`
- Emulator resources: `4096 MB` Android guest RAM, `6 GB` total Docker memory cap, and `2` cores max
- Swap: disabled at the Docker limit and inside Android zram after boot
- `compare-cores`: compatibility alias for the normal 2-core profile
- ADB: `127.0.0.1:15555`
- Emulator console: `127.0.0.1:15554`
- Private ticket test page: `127.0.0.1:19338`
- Persistent data: `/srv/arbuzas/android-sim/`
- Default display profile: aggressive low-load `540x960` at `220` DPI

## Operator Entry Point

Use:

```bash
tools/arbuzas/android-sim.sh preflight --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh launch --variant google-apis --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh validate --variant google-apis --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh vivi-test --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh compare-cores --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh report --variant google-apis --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh reset-state --variant google-apis --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh stop --variant google-apis --ssh-host arbuzas --ssh-user ropepop
```

The default `vivi-test` starts with the lighter Google APIs image and only falls back to Play Store when the first run cannot produce meaningful store-source evidence. AVD state is reused by default; `reset-state` is the deliberate cleanup path.

Aggressive tuning is reapplied by the private tuning loop after emulator restarts. It disables Android zram/swap, limits background processes, enables cached-app freezing where supported, disables scan-always settings, and disables/force-stops nonessential bundled Google apps. If the profile breaks store or ViVi behavior, run `/srv/arbuzas/android-sim/restore-aggressive-packages.sh` on Arbuzas to re-enable the disabled package list.
