# ticket_remote Module Runbook

- Canonical operations: [ROOT_OPERATIONS](./ROOT_OPERATIONS.md)

## Start / Stop / Restart

```bash
../../tools/arbuzas/deploy.sh deploy --services ticket_remote --ssh-host arbuzas --ssh-user "$USER"
../../tools/arbuzas/deploy.sh validate --services ticket_remote --ssh-host arbuzas --ssh-user "$USER"
```

## Health Checks

```bash
curl -fsS http://127.0.0.1:9338/api/v1/health
curl -fsS https://ticket.jolkins.id.lv/api/v1/health
```

The public endpoint sits behind Cloudflare Access for page and API use. A plain unauthenticated public health request may redirect to Access login; use local container health for origin checks.

## Cloudflare Access

Configure a self-hosted Access app for `ticket.jolkins.id.lv`.

- Login method: One-Time PIN / email.
- Policy/session duration: `1 month`.
- Bootstrap admin/member email: `ticket@jolkins.id.lv`.
- Service validates `Cf-Access-Jwt-Assertion`; set the app audience tag in `TICKET_REMOTE_CF_ACCESS_AUDIENCE`.
- SpacetimeDB controls linked ticket membership after Cloudflare confirms identity.

## Pixel Backend

The phone backend is private to Ops through `ticket_phone_bridge`. The bridge connects to the Pixel over ADB on Tailscale, forwards the Pixel's local ticket stream port inside Docker, and exposes it only to `ticket_remote`.
The bridge uses the ADB key files in `/etc/arbuzas/secrets/android-adb/`, mounted read-only into the bridge container. Keep those files scoped to the bridge; they are what let Ops reach the already-authorized Pixel without asking Android to approve a new container identity.

The browser never receives the phone URL. The Pixel keeps AV1, 10 fps, and 1080p-equivalent stream limits.

If the phone leaves ViVi or Android system controls appear, the Pixel backend stops the ticket session; ticket-remote releases controle-code mode and returns viewers to general state.

## Evidence Paths

- `ops/evidence/ticket-remote/`
