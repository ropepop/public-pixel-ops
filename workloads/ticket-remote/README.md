# Ticket Remote

Public manager for `ticket.jolkins.id.lv`.

The service validates Cloudflare Access email identity, checks ticket membership in SpacetimeDB, relays the Pixel ticket stream to viewers, and gates phone inputs through short private controle-code sessions.

## Local Development

```bash
make test
make spacetime-build
TICKET_REMOTE_AUTH_MODE=dev make run
```

## Runtime Model

- General mode: linked users can view the ViVi ticket stream together.
- Controle-code mode: one linked user claims private phone control for 45 seconds.
- One extension is allowed, for 90 seconds total.
- Non-controllers stay connected and see claimant, timer, and presence.
- Browser users never talk directly to the Pixel; only this service talks to the phone backend.

## Required Production Configuration

- `TICKET_REMOTE_CF_ACCESS_TEAM_DOMAIN`
- `TICKET_REMOTE_CF_ACCESS_AUDIENCE`
- `TICKET_REMOTE_STATE_BACKEND=spacetime`
- `TICKET_REMOTE_SPACETIME_DATABASE`
- either `TICKET_REMOTE_SPACETIME_BEARER_TOKEN` or `TICKET_REMOTE_SPACETIME_JWT_PRIVATE_KEY_FILE`
- `TICKET_REMOTE_PHONE_BASE_URL`

Cloudflare Access remains the front door. The public stream uses the existing Cloudflare Tunnel as ordinary HTTPS/WebSocket traffic; there is no WebRTC, TURN, or second public tunnel in this implementation. SpacetimeDB remains the ticket membership and session state source of truth.
