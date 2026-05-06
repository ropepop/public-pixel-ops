// @ts-nocheck
import {
  CaseConversionPolicy,
  SenderError,
  schema,
  table,
  t,
} from 'spacetimedb/server';

const PREFIX = 'ticketremote_';
const CONTROL_MS = 45 * 1000;
const CONTROL_EXTENDED_MS = 90 * 1000;
const PRESENCE_TTL_MS = 45 * 1000;

function named(suffix: string): string {
  return `${PREFIX}${suffix}`;
}

const ticketremote_ticket = table(
  { name: named('ticket') },
  {
    id: t.string().primaryKey(),
    displayName: t.string(),
    createdAt: t.string(),
    updatedAt: t.string(),
  }
);

const ticketremote_ticket_member = table(
  { name: named('ticket_member') },
  {
    id: t.string().primaryKey(),
    ticketId: t.string().index(),
    email: t.string().index(),
    role: t.string().index(),
    active: t.bool(),
    createdAt: t.string(),
    updatedAt: t.string(),
  }
);

const ticketremote_viewer_presence = table(
  { name: named('viewer_presence') },
  {
    sessionId: t.string().primaryKey(),
    ticketId: t.string().index(),
    email: t.string().index(),
    displayName: t.string(),
    page: t.string(),
    connected: t.bool(),
    createdAt: t.string(),
    lastSeenAt: t.string().index(),
  }
);

const ticketremote_control_session = table(
  { name: named('control_session') },
  {
    id: t.string().primaryKey(),
    ticketId: t.string().index(),
    sessionId: t.string().index(),
    email: t.string().index(),
    state: t.string().index(),
    claimedAt: t.string(),
    expiresAt: t.string().index(),
    extended: t.bool(),
    endedAt: t.string(),
    endReason: t.string(),
  }
);

const ticketremote_phone_backend = table(
  { name: named('phone_backend') },
  {
    id: t.string().primaryKey(),
    ticketId: t.string().index(),
    attachName: t.string(),
    baseUrl: t.string(),
    desiredState: t.string(),
    healthJson: t.string(),
    lastError: t.string(),
    lastSeenAt: t.string().index(),
  }
);

const ticketremote_audit_event = table(
  { name: named('audit_event') },
  {
    id: t.string().primaryKey(),
    ticketId: t.string().index(),
    actorEmail: t.string().index(),
    event: t.string().index(),
    payloadJson: t.string(),
    createdAt: t.string().index(),
  }
);

const spacetimedb: any = schema(
  {
    ticketremote_ticket,
    ticketremote_ticket_member,
    ticketremote_viewer_presence,
    ticketremote_control_session,
    ticketremote_phone_backend,
    ticketremote_audit_event,
  },
  { CASE_CONVERSION_POLICY: CaseConversionPolicy.None }
);

export default spacetimedb;

function rowsFrom(iterable: any): any[] {
  return Array.from(iterable as Iterable<any>) as any[];
}

function serialize(payload: unknown): string {
  return JSON.stringify(payload);
}

function cleanEmail(value: string): string {
  return String(value || '').trim().toLowerCase();
}

function cleanRole(value: string): string {
  const role = String(value || '').trim().toLowerCase();
  if (role === 'owner' || role === 'admin') return role;
  return 'member';
}

function parseTime(value: string): number {
  const ms = Date.parse(String(value || ''));
  return Number.isFinite(ms) ? ms : 0;
}

function nowOr(value: string): string {
  const clean = String(value || '').trim();
  return clean || new Date().toISOString();
}

function memberId(ticketId: string, email: string): string {
  return `${ticketId}:${cleanEmail(email)}`;
}

function requireService(tx: any): void {
  const auth = tx.senderAuth;
  const roles = auth?.jwt?.fullPayload?.roles;
  if (!auth?.hasJWT || !Array.isArray(roles) || !roles.includes('ticketremote_service')) {
    throw new SenderError('service role required');
  }
}

function ensureTicket(tx: any, ticketId: string, displayName: string, now: string): any {
  const cleanTicketId = String(ticketId || '').trim() || 'vivi-default';
  const existing = tx.db.ticketremote_ticket.id.find(cleanTicketId);
  if (existing) {
    if (displayName && existing.displayName !== displayName) {
      tx.db.ticketremote_ticket.id.delete(cleanTicketId);
      return tx.db.ticketremote_ticket.insert({ ...existing, displayName, updatedAt: now });
    }
    return existing;
  }
  return tx.db.ticketremote_ticket.insert({
    id: cleanTicketId,
    displayName: displayName || 'ViVi timed ticket',
    createdAt: now,
    updatedAt: now,
  });
}

function isMember(tx: any, ticketId: string, email: string): boolean {
  const member = tx.db.ticketremote_ticket_member.id.find(memberId(ticketId, email));
  return Boolean(member && member.active === true);
}

function isAdmin(tx: any, ticketId: string, email: string): boolean {
  const member = tx.db.ticketremote_ticket_member.id.find(memberId(ticketId, email));
  return Boolean(member && member.active === true && (member.role === 'owner' || member.role === 'admin'));
}

function audit(tx: any, ticketId: string, actorEmail: string, event: string, payloadJson: string, now: string): void {
  const ordinal = rowsFrom(tx.db.ticketremote_audit_event.ticketId.filter(ticketId)).length + 1;
  const stamp = String(now || '').replace(/[^0-9A-Za-z]/g, '') || 'time';
  const cleanEvent = String(event || 'event').replace(/[^0-9A-Za-z_-]/g, '_');
  tx.db.ticketremote_audit_event.insert({
    id: `${ticketId}:${stamp}:${ordinal}:${cleanEvent}`,
    ticketId,
    actorEmail: cleanEmail(actorEmail),
    event: String(event || '').trim(),
    payloadJson: payloadJson || '{}',
    createdAt: now,
  });
}

function clearPhoneBackends(tx: any, ticketId: string): void {
  for (const row of rowsFrom(tx.db.ticketremote_phone_backend.ticketId.filter(ticketId))) {
    tx.db.ticketremote_phone_backend.id.delete(row.id);
  }
}

function cleanup(tx: any, ticketId: string, now: string): void {
  const nowMs = parseTime(now);
  for (const row of rowsFrom(tx.db.ticketremote_viewer_presence.ticketId.filter(ticketId))) {
    if (nowMs - parseTime(row.lastSeenAt) > PRESENCE_TTL_MS) {
      tx.db.ticketremote_viewer_presence.sessionId.delete(row.sessionId);
    }
  }
  for (const row of rowsFrom(tx.db.ticketremote_control_session.ticketId.filter(ticketId))) {
    if (row.state === 'active' && parseTime(row.expiresAt) <= nowMs) {
      tx.db.ticketremote_control_session.id.delete(row.id);
      tx.db.ticketremote_control_session.insert({
        ...row,
        state: 'expired',
        endedAt: now,
        endReason: 'timeout',
      });
      audit(tx, ticketId, row.email, 'control_expired', serialize({ sessionId: row.sessionId }), now);
    }
  }
}

function activeControl(tx: any, ticketId: string, now: string): any | null {
  cleanup(tx, ticketId, now);
  const nowMs = parseTime(now);
  const rows = rowsFrom(tx.db.ticketremote_control_session.ticketId.filter(ticketId))
    .filter((row) => row.state === 'active')
    .sort((a, b) => parseTime(a.expiresAt) - parseTime(b.expiresAt));
  const row = rows[0];
  if (!row) return null;
  return {
    id: row.id,
    sessionId: row.sessionId,
    email: row.email,
    claimedAt: row.claimedAt,
    expiresAt: row.expiresAt,
    extended: row.extended,
    remainingMs: Math.max(0, parseTime(row.expiresAt) - nowMs),
  };
}

function snapshot(tx: any, ticketId: string, now: string): string {
  const ticket = ensureTicket(tx, ticketId, '', now);
  cleanup(tx, ticket.id, now);
  const members = rowsFrom(tx.db.ticketremote_ticket_member.ticketId.filter(ticket.id))
    .map((row) => ({
      email: row.email,
      role: row.role,
      active: row.active,
      updatedAt: row.updatedAt,
    }))
    .sort((a, b) => a.email.localeCompare(b.email));
  const viewers = rowsFrom(tx.db.ticketremote_viewer_presence.ticketId.filter(ticket.id))
    .filter((row) => row.connected === true)
    .map((row) => ({
      sessionId: row.sessionId,
      email: row.email,
      displayName: row.displayName,
      page: row.page,
      connected: row.connected,
      lastSeenAt: row.lastSeenAt,
    }))
    .sort((a, b) => a.email.localeCompare(b.email));
  const phone = rowsFrom(tx.db.ticketremote_phone_backend.ticketId.filter(ticket.id))[0] || null;
  return serialize({
    ok: true,
    state: {
      ticket: { id: ticket.id, displayName: ticket.displayName, updatedAt: ticket.updatedAt },
      members,
      viewers,
      activeControl: activeControl(tx, ticket.id, now),
      phone: phone ? {
        id: phone.id,
        attachName: phone.attachName,
        baseUrl: phone.baseUrl,
        desiredState: phone.desiredState,
        healthJson: phone.healthJson,
        lastError: phone.lastError,
        lastSeenAt: phone.lastSeenAt,
      } : null,
      serverTime: now,
      stateBackend: 'spacetime',
    }
  });
}

function stateError(tx: any, ticketId: string, code: string, now: string): string {
  return serialize({ ok: false, error: code, state: JSON.parse(snapshot(tx, ticketId, now)).state });
}

export const serviceBootstrap = spacetimedb.reducer(
  { name: named('service_bootstrap') },
  { ticketId: t.string(), displayName: t.string(), adminEmail: t.string(), phoneBackendId: t.string(), phoneBaseUrl: t.string(), phoneAttachName: t.string() },
  (ctx, args) => {
    const tx = ctx;
    requireService(tx);
    const now = new Date().toISOString();
    const ticket = ensureTicket(tx, args.ticketId, args.displayName, now);
    const email = cleanEmail(args.adminEmail);
    if (email) {
      const id = memberId(ticket.id, email);
      const existing = tx.db.ticketremote_ticket_member.id.find(id);
      if (!existing) {
        tx.db.ticketremote_ticket_member.insert({ id, ticketId: ticket.id, email, role: 'owner', active: true, createdAt: now, updatedAt: now });
      }
    }
    if (String(args.phoneBackendId || '').trim()) {
      const id = String(args.phoneBackendId).trim();
      clearPhoneBackends(tx, ticket.id);
      tx.db.ticketremote_phone_backend.insert({
        id,
        ticketId: ticket.id,
        attachName: String(args.phoneAttachName || '').trim() || id,
        baseUrl: String(args.phoneBaseUrl || '').trim(),
        desiredState: 'idle',
        healthJson: '',
        lastError: '',
        lastSeenAt: now,
      });
    }
  }
);

export const getState = spacetimedb.procedure(
  { name: named('get_state') },
  { ticketId: t.string(), now: t.string() },
  t.string(),
  (ctx, { ticketId, now }) => ctx.withTx((tx) => {
    requireService(tx);
    return snapshot(tx, ticketId, nowOr(now));
  })
);

export const upsertMember = spacetimedb.procedure(
  { name: named('upsert_member') },
  { ticketId: t.string(), actorEmail: t.string(), email: t.string(), role: t.string(), now: t.string() },
  t.string(),
  (ctx, args) => ctx.withTx((tx) => {
    requireService(tx);
    const now = nowOr(args.now);
    const ticket = ensureTicket(tx, args.ticketId, '', now);
    if (!isAdmin(tx, ticket.id, args.actorEmail)) return stateError(tx, ticket.id, 'forbidden', now);
    const email = cleanEmail(args.email);
    const id = memberId(ticket.id, email);
    const existing = tx.db.ticketremote_ticket_member.id.find(id);
    if (existing) tx.db.ticketremote_ticket_member.id.delete(id);
    tx.db.ticketremote_ticket_member.insert({ id, ticketId: ticket.id, email, role: cleanRole(args.role), active: true, createdAt: existing?.createdAt || now, updatedAt: now });
    audit(tx, ticket.id, args.actorEmail, 'member_upserted', serialize({ email, role: cleanRole(args.role) }), now);
    return snapshot(tx, ticket.id, now);
  })
);

export const removeMember = spacetimedb.procedure(
  { name: named('remove_member') },
  { ticketId: t.string(), actorEmail: t.string(), email: t.string(), now: t.string() },
  t.string(),
  (ctx, args) => ctx.withTx((tx) => {
    requireService(tx);
    const now = nowOr(args.now);
    const ticket = ensureTicket(tx, args.ticketId, '', now);
    if (!isAdmin(tx, ticket.id, args.actorEmail)) return stateError(tx, ticket.id, 'forbidden', now);
    const id = memberId(ticket.id, args.email);
    const existing = tx.db.ticketremote_ticket_member.id.find(id);
    if (existing) {
      tx.db.ticketremote_ticket_member.id.delete(id);
      tx.db.ticketremote_ticket_member.insert({ ...existing, active: false, updatedAt: now });
    }
    audit(tx, ticket.id, args.actorEmail, 'member_removed', serialize({ email: cleanEmail(args.email) }), now);
    return snapshot(tx, ticket.id, now);
  })
);

export const heartbeatPresence = spacetimedb.procedure(
  { name: named('heartbeat_presence') },
  { ticketId: t.string(), sessionId: t.string(), email: t.string(), displayName: t.string(), page: t.string(), connected: t.bool(), now: t.string() },
  t.string(),
  (ctx, args) => ctx.withTx((tx) => {
    requireService(tx);
    const now = nowOr(args.now);
    const ticket = ensureTicket(tx, args.ticketId, '', now);
    if (!isMember(tx, ticket.id, args.email)) return stateError(tx, ticket.id, 'not_member', now);
    const existing = tx.db.ticketremote_viewer_presence.sessionId.find(args.sessionId);
    if (existing) tx.db.ticketremote_viewer_presence.sessionId.delete(args.sessionId);
    tx.db.ticketremote_viewer_presence.insert({
      sessionId: args.sessionId,
      ticketId: ticket.id,
      email: cleanEmail(args.email),
      displayName: String(args.displayName || '').trim() || cleanEmail(args.email),
      page: String(args.page || '').trim() || 'ticket',
      connected: args.connected === true,
      createdAt: existing?.createdAt || now,
      lastSeenAt: now,
    });
    return snapshot(tx, ticket.id, now);
  })
);

export const disconnectPresence = spacetimedb.procedure(
  { name: named('disconnect_presence') },
  { ticketId: t.string(), sessionId: t.string(), now: t.string() },
  t.string(),
  (ctx, args) => ctx.withTx((tx) => {
    requireService(tx);
    const now = nowOr(args.now);
    const existing = tx.db.ticketremote_viewer_presence.sessionId.find(args.sessionId);
    if (existing) {
      tx.db.ticketremote_viewer_presence.sessionId.delete(args.sessionId);
      tx.db.ticketremote_viewer_presence.insert({ ...existing, connected: false, lastSeenAt: now });
    }
    return snapshot(tx, args.ticketId, now);
  })
);

export const claimControl = spacetimedb.procedure(
  { name: named('claim_control') },
  { ticketId: t.string(), sessionId: t.string(), email: t.string(), now: t.string() },
  t.string(),
  (ctx, args) => ctx.withTx((tx) => {
    requireService(tx);
    const now = nowOr(args.now);
    const ticket = ensureTicket(tx, args.ticketId, '', now);
    if (!isMember(tx, ticket.id, args.email)) return stateError(tx, ticket.id, 'not_member', now);
    if (activeControl(tx, ticket.id, now)) return stateError(tx, ticket.id, 'control_claimed', now);
    const nowMs = parseTime(now);
    const id = `${args.sessionId}-${nowMs}`;
    tx.db.ticketremote_control_session.insert({
      id,
      ticketId: ticket.id,
      sessionId: args.sessionId,
      email: cleanEmail(args.email),
      state: 'active',
      claimedAt: now,
      expiresAt: new Date(nowMs + CONTROL_MS).toISOString(),
      extended: false,
      endedAt: '',
      endReason: '',
    });
    audit(tx, ticket.id, args.email, 'control_claimed', serialize({ sessionId: args.sessionId }), now);
    return snapshot(tx, ticket.id, now);
  })
);

export const extendControl = spacetimedb.procedure(
  { name: named('extend_control') },
  { ticketId: t.string(), sessionId: t.string(), email: t.string(), now: t.string() },
  t.string(),
  (ctx, args) => ctx.withTx((tx) => {
    requireService(tx);
    const now = nowOr(args.now);
    const ticket = ensureTicket(tx, args.ticketId, '', now);
    const active = rowsFrom(tx.db.ticketremote_control_session.ticketId.filter(ticket.id)).find((row) => row.state === 'active');
    if (!active) return stateError(tx, ticket.id, 'no_control', now);
    if (active.sessionId !== args.sessionId || active.email !== cleanEmail(args.email)) return stateError(tx, ticket.id, 'not_controller', now);
    if (active.extended === true) return stateError(tx, ticket.id, 'already_extended', now);
    tx.db.ticketremote_control_session.id.delete(active.id);
    tx.db.ticketremote_control_session.insert({ ...active, extended: true, expiresAt: new Date(parseTime(active.claimedAt) + CONTROL_EXTENDED_MS).toISOString() });
    audit(tx, ticket.id, args.email, 'control_extended', serialize({ sessionId: args.sessionId }), now);
    return snapshot(tx, ticket.id, now);
  })
);

export const releaseControl = spacetimedb.procedure(
  { name: named('release_control') },
  { ticketId: t.string(), sessionId: t.string(), email: t.string(), reason: t.string(), now: t.string() },
  t.string(),
  (ctx, args) => ctx.withTx((tx) => {
    requireService(tx);
    const now = nowOr(args.now);
    const ticket = ensureTicket(tx, args.ticketId, '', now);
    const active = rowsFrom(tx.db.ticketremote_control_session.ticketId.filter(ticket.id)).find((row) => row.state === 'active');
    if (!active) return snapshot(tx, ticket.id, now);
    if (args.sessionId && (active.sessionId !== args.sessionId || active.email !== cleanEmail(args.email))) return stateError(tx, ticket.id, 'not_controller', now);
    tx.db.ticketremote_control_session.id.delete(active.id);
    tx.db.ticketremote_control_session.insert({ ...active, state: 'released', endedAt: now, endReason: String(args.reason || 'released') });
    audit(tx, ticket.id, args.email || active.email, 'control_released', serialize({ reason: args.reason }), now);
    return snapshot(tx, ticket.id, now);
  })
);

export const revokeControl = spacetimedb.procedure(
  { name: named('revoke_control') },
  { ticketId: t.string(), actorEmail: t.string(), reason: t.string(), now: t.string() },
  t.string(),
  (ctx, args) => ctx.withTx((tx) => {
    requireService(tx);
    const now = nowOr(args.now);
    const ticket = ensureTicket(tx, args.ticketId, '', now);
    if (!isAdmin(tx, ticket.id, args.actorEmail)) return stateError(tx, ticket.id, 'forbidden', now);
    for (const active of rowsFrom(tx.db.ticketremote_control_session.ticketId.filter(ticket.id)).filter((row) => row.state === 'active')) {
      tx.db.ticketremote_control_session.id.delete(active.id);
      tx.db.ticketremote_control_session.insert({ ...active, state: 'revoked', endedAt: now, endReason: String(args.reason || 'admin_revoked') });
    }
    audit(tx, ticket.id, args.actorEmail, 'control_revoked', serialize({ reason: args.reason }), now);
    return snapshot(tx, ticket.id, now);
  })
);

export const updatePhone = spacetimedb.procedure(
  { name: named('update_phone') },
  { ticketId: t.string(), backendId: t.string(), attachName: t.string(), baseUrl: t.string(), desiredState: t.string(), healthJson: t.string(), lastError: t.string(), now: t.string() },
  t.string(),
  (ctx, args) => ctx.withTx((tx) => {
    requireService(tx);
    const now = nowOr(args.now);
    const ticket = ensureTicket(tx, args.ticketId, '', now);
    const id = String(args.backendId || '').trim() || 'pixel';
    clearPhoneBackends(tx, ticket.id);
    tx.db.ticketremote_phone_backend.insert({
      id,
      ticketId: ticket.id,
      attachName: String(args.attachName || '').trim() || id,
      baseUrl: String(args.baseUrl || '').trim(),
      desiredState: String(args.desiredState || '').trim() || 'idle',
      healthJson: String(args.healthJson || ''),
      lastError: String(args.lastError || ''),
      lastSeenAt: now,
    });
    return snapshot(tx, ticket.id, now);
  })
);

export const auditEvent = spacetimedb.reducer(
  { name: named('audit') },
  { ticketId: t.string(), actorEmail: t.string(), event: t.string(), payloadJson: t.string(), now: t.string() },
  (ctx, args) => {
    const tx = ctx;
    requireService(tx);
    audit(tx, args.ticketId, args.actorEmail, args.event, args.payloadJson || '{}', nowOr(args.now));
  }
);
