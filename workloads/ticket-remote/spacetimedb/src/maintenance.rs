//! A release pause blocks new phone work while admitted work can still settle.
use super::*;

#[spacetimedb::table(accessor = ticketremote_maintenance)]
#[derive(Clone)]
pub struct TicketremoteMaintenance {
    #[primary_key]
    pub ticketId: String,
    pub paused: bool,
    pub updatedAt: String,
}

/// The pinned database operator may control this gate only; it gains no other
/// member/service mutation authority. Active product owners may also control it.
#[spacetimedb::reducer]
pub fn ticketremote_owner_set_maintenance(
    ctx: &ReducerContext,
    ticketId: String,
    paused: bool,
) -> Result<(), String> {
    let ticket_id = clean_ticket_id(&ticketId);
    if !operator_identity_is_valid(&ctx.sender().to_string()) {
        let email = client_email_from_auth(ctx, &ticket_id)?;
        require_owner(ctx, &ticket_id, &email)?;
    }
    let row = TicketremoteMaintenance {
        ticketId: ticket_id,
        paused,
        updatedAt: now(ctx),
    };
    let table = ctx.db.ticketremote_maintenance();
    if table.ticketId().find(&row.ticketId).is_some() {
        table.ticketId().update(row);
    } else {
        table.insert(row);
    }
    Ok(())
}

pub(super) fn maintenance_paused(ctx: &ReducerContext, ticket_id: &str) -> bool {
    ctx.db
        .ticketremote_maintenance()
        .ticketId()
        .find(clean_ticket_id(ticket_id))
        .is_some_and(|row| row.paused)
}

pub(super) fn require_new_phone_admission(
    ctx: &ReducerContext,
    ticket_id: &str,
) -> Result<(), String> {
    if cold_restart::ticket_blocked(ctx, ticket_id) {
        Err("ticket_cold_restart".into())
    } else if maintenance_paused(ctx, ticket_id) {
        Err("ticket_maintenance".into())
    } else {
        Ok(())
    }
}
