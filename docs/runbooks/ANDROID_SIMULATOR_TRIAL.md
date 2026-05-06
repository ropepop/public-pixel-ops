# Android Simulator Trial

This is the private Arbuzas lab for testing whether a Docker-hosted Android emulator can replace a physical phone for the ViVi ticket flow.

The lab script is separate from the normal public Arbuzas release and does not change `ticket.jolkins.id.lv`.
The normal ticket stack also has a persistent Docker-private Android simulator service; use this lab when you need resettable experiments, evidence bundles, or repeatable simulator checks.

## Fixed Trial Target

- Host: Arbuzas
- Runtime: `HQarroum/docker-android`
- First image: `halimqarroum/docker-android:api-33`
- Second image: `halimqarroum/docker-android:api-33-playstore`
- Resources: `4096 MB` Android guest RAM, `6 GB` total Docker memory cap, `2` cores max
- Swap: disabled at the Docker limit and inside Android zram after boot
- ADB: `127.0.0.1:15555`
- Emulator console: `127.0.0.1:15554`
- Private ticket test service: `127.0.0.1:19338`
- State and evidence: `/srv/arbuzas/android-sim/`
- Default display tuning: aggressive low-load `540x960` at `220` DPI, with animations disabled

## Commands

```bash
tools/arbuzas/android-sim.sh preflight --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh launch --variant google-apis --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh validate --variant google-apis --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh benchmark --variant google-apis --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh reset-state --variant google-apis --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh stop --variant google-apis --ssh-host arbuzas --ssh-user ropepop
```

The emulator reuses `/srv/arbuzas/android-sim/<variant>/avd` by default so repeat runs are warm. Use `reset-state` only when a fresh emulator state is needed. Reports record whether the run started from warm or fresh state.

For the richer no-login ViVi acquisition and responsiveness run, use:

```bash
tools/arbuzas/android-sim.sh vivi-test --ssh-host arbuzas --ssh-user ropepop
```

`vivi-test` defaults to the lighter Google APIs image, captures screenshots plus UI timing evidence, validates the private ticket test service, and stops the private stack afterward. It falls back to the Play Store image only if the Google APIs run cannot reach a meaningful store-source result.

`compare-cores` and `vivi-test --comparison-cores 2` are compatibility aliases for the normal 2-core no-swap profile:

```bash
tools/arbuzas/android-sim.sh compare-cores --ssh-host arbuzas --ssh-user ropepop
tools/arbuzas/android-sim.sh vivi-test --comparison-cores 2 --ssh-host arbuzas --ssh-user ropepop
```

The compatibility path does not change the normal 2-core default.

Aggressive tuning is now handled by a private tuning loop so it is reapplied after emulator restarts. It disables Android zram/swap, limits Android background work, enables cached-app freezing where supported, disables scan-always settings, and disables/force-stops nonessential bundled Google apps. It preserves SystemUI, Settings, Package Installer, WebView, GMS/GSF, Pixel Launcher, Accrescent, Aurora, ViVi, and the ticket controller. If the profile breaks store or ViVi behavior, run `/srv/arbuzas/android-sim/restore-aggressive-packages.sh` on Arbuzas to re-enable the disabled package list.

## ViVi App Source Rule

The simulator installs Accrescent from `https://accrescent.app/accrescent.apk` and Aurora Store from F-Droid's Aurora Store package.

Store APKs are cached under `/srv/arbuzas/android-sim/apks/` and downloaded again only when missing or older than 7 days. Store clients are installed only if their packages are missing, unless `--force-store-install` is passed.

The script opens Accrescent first for `com.pv.vivi`, then Aurora with a `market://details?id=com.pv.vivi` intent. It does not use random APK mirrors and it does not sign in to Google or ViVi.

Success means:

- `com.pv.vivi` is installed,
- Android can launch the package,
- ViVi reaches a visible first screen.

If either store requires manual in-app taps or the app is unavailable, the result is recorded as blocked instead of bypassing the store-source rule.

The `vivi-test` action is allowed to tap visible no-login store UI controls such as install, continue, anonymous, skip, or later. It must not tap sign-in or account-login controls.

## Evidence To Compare With A Real Phone

Use the benchmark report to compare:

- boot time,
- ADB readiness,
- Android installer readiness,
- warm or fresh emulator state,
- display size and DPI,
- store install time,
- ViVi availability and launch state,
- launcher response time,
- store open and tap-to-screen-change timing,
- private ticket service health,
- CPU and memory pressure,
- disk growth,
- temperature.

Netdata can still be used for host graphs, but the simulator report is the canonical trial artifact.
