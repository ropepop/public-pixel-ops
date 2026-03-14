# Command Reference

## Primary Entry Points

- `tools/pixel/redeploy.sh`
  - smart wrapper for scoped or full deploys
- `tools/pixel/check_ssh_ready.sh`
  - SSH readiness check before an SSH-first deploy
- `orchestrator/scripts/android/pixel_redeploy.sh`
  - orchestrator-side implementation used by the wrapper

## Common Flows

Validate only:

```bash
./tools/pixel/redeploy.sh --mode validate-only --transport ssh --ssh-host "${PIXEL_SSH_HOST}"
```

Full deploy:

```bash
./tools/pixel/redeploy.sh --scope full --transport ssh --ssh-host "${PIXEL_SSH_HOST}"
```

Single component deploy:

```bash
./tools/pixel/redeploy.sh --scope train_bot --transport ssh --ssh-host "${PIXEL_SSH_HOST}"
./tools/pixel/redeploy.sh --scope satiksme_bot --transport ssh --ssh-host "${PIXEL_SSH_HOST}"
./tools/pixel/redeploy.sh --scope site_notifier --transport ssh --ssh-host "${PIXEL_SSH_HOST}"
```

ADB fallback:

```bash
./tools/pixel/redeploy.sh --scope full --transport adb --device "${ADB_SERIAL}"
```
