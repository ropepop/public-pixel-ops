//! A single durable barrier owns a deliberate cold restart across relay and Pixel.
use super::*;

const SHUTDOWN_DEADLINE_MS: i64 = 15_000;

pub(super) fn predates_boundary(ctx: &ReducerContext, ticket: &str, backend: &str, clock: &str) -> bool {
    ctx.db.ticketremote_stream_desired_state().id().find(phone_row_id(ticket, backend))
        .and_then(|row| row.coldRestartStartedAt)
        .is_some_and(|start| parse_time_ms(clock) <= parse_time_ms(&start))
}

pub(super) fn blocks(phase: Option<&str>) -> bool {
    matches!(phase, Some("quiescing" | "stopping" | "confirmed" | "failed"))
}

pub(super) fn ticket_blocked(ctx: &ReducerContext, ticket_id: &str) -> bool {
    let ticket = clean_ticket_id(ticket_id);
    ctx.db.ticketremote_stream_desired_state().ticketBackend()
        .filter((&ticket,))
        .any(|row| blocks(row.coldRestartPhase.as_deref()))
}

fn within_deadline(row: &TicketremoteStreamDesiredState, clock: &str) -> bool {
    let start = parse_time_ms(row.coldRestartStartedAt.as_deref().unwrap_or(""));
    let age = parse_time_ms(clock).saturating_sub(start);
    start > 0 && (0..SHUTDOWN_DEADLINE_MS).contains(&age)
}

fn save(ctx: &ReducerContext, row: TicketremoteStreamDesiredState) {
    let signal = (row.ticketId.clone(), row.backendId.clone(), row.revision.clone(), row.updatedAt.clone());
    ctx.db.ticketremote_stream_desired_state().id().update(row);
    upsert_stream_command_signal(ctx, &signal.0, &signal.1, &signal.2, &signal.3);
}

#[spacetimedb::reducer]
pub fn ticketremote_begin_cold_restart(
    ctx: &ReducerContext, ticketId: String, backendId: String, operationId: String, actorEmail: String,
) -> Result<(), String> {
    require_service(ctx)?;
    let clock = now(ctx);
    let ticket = ensure_ticket(ctx, &ticketId, "", &clock);
    require_owner(ctx, &ticket.id, &clean_email(&actorEmail))?;
    if operationId.trim().is_empty() || operationId.len() > 100 { return Err("invalid_cold_restart_id".into()); }
    let backend = clean_backend_id(&backendId);
    let prior = ctx.db.ticketremote_stream_desired_state().id().find(phone_row_id(&ticket.id, &backend));
    if prior.as_ref().is_some_and(|row| row.coldRestartId.as_deref() == Some(operationId.as_str())) { return Ok(()); }
    if prior.as_ref().is_some_and(|row| matches!(row.coldRestartPhase.as_deref(),
        Some("quiescing" | "stopping" | "confirmed" | "reloading"))) { return Ok(()); }
    require_new_phone_admission_unless_failed(ctx, &ticket.id)?;
    if let Some(reason) = ticket_phone_mutation_lane_conflict(ctx, &ticket.id, &backend, &clock) {
        return Err(reason.into());
    }
    if ctx.db.ticketremote_ticket_action_v3_queued_intent().id().find(ticket_action_v3_queue_id(&ticket.id, &backend)).is_some() {
        return Err("phone_work_queued".into());
    }
    // The barrier and cancellation of queued background starts commit together.
    let mut row = if let Some(row) = prior { row } else {
        upsert_stream_desired_state(ctx, &ticket.id, &backend, false, 0, "cold_restart", &operationId, "owner", &clock)
    };
    row.desiredActive = false;
    row.viewerCount = 0;
    row.reason = "cold_restart".into();
    row.revision = operationId.clone();
    row.updatedAt = clock.clone();
    row.updatedBy = "owner".into();
    row.coldRestartId = Some(operationId.clone());
    row.coldRestartPhase = Some("quiescing".into());
    row.coldRestartStartedAt = Some(clock.clone());
    row.coldRestartError = None;
    purge_pending_idle_background_commands(ctx, &ticket.id, &backend, &operationId, &clock);
    save(ctx, row);
    Ok(())
}

fn require_new_phone_admission_unless_failed(ctx: &ReducerContext, ticket: &str) -> Result<(), String> {
    if maintenance_paused(ctx, ticket) { Err("ticket_maintenance".into()) } else { Ok(()) }
}

#[spacetimedb::reducer]
pub fn ticketremote_advance_cold_restart(
    ctx: &ReducerContext, ticketId: String, backendId: String, operationId: String, phase: String,
) -> Result<(), String> {
    require_service(ctx)?;
    let mut row = ctx.db.ticketremote_stream_desired_state().id()
        .find(phone_row_id(&clean_ticket_id(&ticketId), &clean_backend_id(&backendId)))
        .ok_or("cold_restart_missing")?;
    if row.coldRestartId.as_deref() != Some(operationId.as_str()) { return Err("cold_restart_superseded".into()); }
    let clock = now(ctx);
    if phase == "failed" && !blocks(row.coldRestartPhase.as_deref()) { return Err("cold_restart_already_released".into()); }
    if phase == "failed" || (blocks(row.coldRestartPhase.as_deref()) && !within_deadline(&row, &clock)) {
        row.coldRestartPhase = Some("failed".into());
        row.coldRestartError = Some("cold_shutdown_unproved".into());
    } else {
        match (row.coldRestartPhase.as_deref(), phase.as_str()) {
            (Some("quiescing"), "stopping") => {
                insert_stream_command(ctx, &row.ticketId, &row.backendId,
                    &format!("cold-stop:{operationId}"), "cold_stop", &operationId, "owner_cold_restart",
                    "{}", SHUTDOWN_DEADLINE_MS - (parse_time_ms(&clock) - parse_time_ms(row.coldRestartStartedAt.as_deref().unwrap_or(""))), &clock);
                row.coldRestartPhase = Some("stopping".into());
            }
            (Some("confirmed"), "reloading" | "asleep") => row.coldRestartPhase = Some(phase),
            (Some("reloading"), "live") => row.coldRestartPhase = Some(phase),
            (Some(current), requested) if current == requested => return Ok(()),
            _ => return Err("cold_restart_invalid_transition".into()),
        }
    }
    row.updatedAt = clock;
    save(ctx, row);
    Ok(())
}

pub(super) fn acknowledge(ctx: &ReducerContext, command: &TicketremoteStreamCommand, status: &str, reason: &str, clock: &str) {
    if command.commandType != "cold_stop" || status == "dispatched" { return; }
    let Some(mut row) = ctx.db.ticketremote_stream_desired_state().id()
        .find(phone_row_id(&command.ticketId, &command.backendId)) else { return; };
    if row.coldRestartId.as_deref() != Some(command.revision.as_str()) || row.coldRestartPhase.as_deref() != Some("stopping") { return; }
    let proved = status == "acknowledged" && reason == "cold_capture_released" && within_deadline(&row, clock);
    row.coldRestartPhase = Some(if proved { "confirmed" } else { "failed" }.into());
    row.coldRestartError = if proved { None } else { Some("cold_shutdown_unproved".into()) };
    row.updatedAt = clock.into();
    save(ctx, row);
}

pub(super) fn note_live_report(ctx: &ReducerContext, ticket: &str, backend: &str, verdict: &str, frame_at: &str, clock: &str) {
    if verdict != "live" { return; }
    let Some(mut row) = ctx.db.ticketremote_stream_desired_state().id().find(phone_row_id(ticket, backend)) else { return; };
    if !matches!(row.coldRestartPhase.as_deref(), Some("reloading" | "asleep")) || parse_time_ms(frame_at) <= parse_time_ms(&row.updatedAt) { return; }
    row.coldRestartPhase = Some("live".into());
    row.updatedAt = clock.into();
    save(ctx, row);
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn failures_keep_admission_closed_until_explicit_retry() {
        for phase in ["quiescing", "stopping", "confirmed", "failed"] { assert!(blocks(Some(phase))); }
        for phase in ["reloading", "asleep", "live"] { assert!(!blocks(Some(phase))); }
        assert!(!blocks(None));
    }
}
