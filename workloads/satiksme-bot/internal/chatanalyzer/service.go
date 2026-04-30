package chatanalyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"satiksmebot/internal/model"
	"satiksmebot/internal/reports"
	"satiksmebot/internal/store"
)

type Service struct {
	settings    Settings
	store       store.ChatAnalyzerStore
	collector   Collector
	analyzer    BatchAnalyzer
	catalog     CatalogProvider
	reports     *reports.Service
	dump        ReportDumper
	liveFetcher LiveVehicleFetcher
	incidents   ActiveIncidentFetcher
	now         func() time.Time

	consecutiveModelFailures int
	modelCircuitOpenUntil    time.Time
}

type ServiceOptions struct {
	Settings    Settings
	Store       store.ChatAnalyzerStore
	Collector   Collector
	Analyzer    BatchAnalyzer
	Catalog     CatalogProvider
	Reports     *reports.Service
	Dump        ReportDumper
	LiveFetcher LiveVehicleFetcher
	Incidents   ActiveIncidentFetcher
	Now         func() time.Time
}

type RunOnceResult struct {
	Collected int
	Processed bool
	RetryAt   time.Time
	Batch     *model.ChatAnalyzerBatch
}

func NewService(opts ServiceOptions) *Service {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		settings:    opts.Settings.withDefaults(),
		store:       opts.Store,
		collector:   opts.Collector,
		analyzer:    opts.Analyzer,
		catalog:     opts.Catalog,
		reports:     opts.Reports,
		dump:        opts.Dump,
		liveFetcher: opts.LiveFetcher,
		incidents:   opts.Incidents,
		now:         now,
	}
}

func (s *Service) Run(ctx context.Context) error {
	if s == nil || !s.settings.Enabled {
		return nil
	}
	if s.store == nil || s.collector == nil || s.analyzer == nil || s.catalog == nil || s.reports == nil {
		return fmt.Errorf("chat analyzer is enabled but not fully configured")
	}
	nextCollect := time.Time{}
	nextProcess := nextScheduledProcessAt(s.now().UTC(), s.settings)
	lastProcessAt := time.Time{}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			now := s.now().UTC()
			if !now.Before(nextCollect) {
				collected, err := s.collectNewMessages(ctx)
				if err != nil {
					log.Printf("satiksme chat analyzer collect failed: %v", err)
				} else if collected > 0 {
					if eventProcessAt := s.throttledProcessAt(now, lastProcessAt); nextProcess.IsZero() || eventProcessAt.Before(nextProcess) {
						nextProcess = eventProcessAt
					}
				}
				nextCollect = now.Add(s.settings.PollInterval)
			}
			var nextRetry time.Time
			if !now.Before(nextProcess) {
				processed, retryAt, _, err := s.processPendingBatch(ctx)
				if err != nil {
					log.Printf("satiksme chat analyzer pass failed: %v", err)
				}
				if processed {
					lastProcessAt = now
				}
				nextProcess = s.throttledProcessAt(nextScheduledProcessAfter(now, s.settings), lastProcessAt)
				if !retryAt.IsZero() {
					retryProcessAt := s.throttledProcessAt(retryAt, lastProcessAt)
					nextRetry = retryProcessAt
					if nextProcess.IsZero() || retryProcessAt.Before(nextProcess) {
						nextProcess = retryProcessAt
					}
				}
			}
			timer.Reset(s.nextDelay(now, nextCollect, nextProcess, nextRetry))
		}
	}
}

func (s *Service) RunOnce(ctx context.Context) error {
	_, err := s.RunOnceWithResult(ctx)
	return err
}

func (s *Service) RunOnceWithResult(ctx context.Context) (RunOnceResult, error) {
	collected, err := s.collectNewMessages(ctx)
	if err != nil {
		return RunOnceResult{}, err
	}
	processed, retryAt, batch, err := s.processPendingBatch(ctx)
	return RunOnceResult{
		Collected: collected,
		Processed: processed,
		RetryAt:   retryAt,
		Batch:     batch,
	}, err
}

func (s *Service) collectNewMessages(ctx context.Context) (int, error) {
	collected, err := s.collector.Collect(ctx)
	if err != nil {
		return 0, fmt.Errorf("collect telegram chat: %w", err)
	}
	collectedMax := make(map[string]int64)
	for _, item := range collected {
		if _, err := s.store.EnqueueChatAnalyzerMessage(ctx, item); err != nil {
			return 0, fmt.Errorf("enqueue telegram chat message %s: %w", item.ID, err)
		}
		if item.MessageID > collectedMax[item.ChatID] {
			collectedMax[item.ChatID] = item.MessageID
		}
	}
	for chatID, messageID := range collectedMax {
		if err := s.store.SetChatAnalyzerCheckpoint(ctx, chatID, messageID, s.now().UTC()); err != nil {
			return 0, fmt.Errorf("advance telegram chat checkpoint %s: %w", chatID, err)
		}
	}
	return len(collected), nil
}

func (s *Service) processPendingBatch(ctx context.Context) (bool, time.Time, *model.ChatAnalyzerBatch, error) {
	pending, err := s.store.ListPendingChatAnalyzerMessages(ctx, s.settings.BatchLimit)
	if err != nil {
		return false, time.Time{}, nil, fmt.Errorf("list pending telegram chat messages: %w", err)
	}
	if len(pending) == 0 {
		return false, time.Time{}, nil, nil
	}

	now := s.now().UTC()
	if s.modelCircuitOpenUntil.After(now) {
		return false, s.modelCircuitOpenUntil, nil, nil
	}
	ready := make([]model.ChatAnalyzerMessage, 0, len(pending))
	var nextRetry time.Time
	for i := range pending {
		item := pending[i]
		if s.messageReadyForRetry(item, now) {
			ready = append(ready, pending[i])
			continue
		}
		retryAt := item.ProcessedAt.Add(s.retryDelay(item.Attempts))
		if nextRetry.IsZero() || retryAt.Before(nextRetry) {
			nextRetry = retryAt
		}
	}
	if len(ready) == 0 {
		return false, nextRetry, nil, nil
	}

	catalog := s.catalog.Current()
	vehicles := s.fetchLiveVehicles(ctx, catalog, now)
	incidents, err := s.activeIncidents(ctx, catalog, now)
	if err != nil {
		return false, time.Time{}, nil, fmt.Errorf("load incident candidates: %w", err)
	}
	batch, err := s.processBatch(ctx, catalog, vehicles, incidents, ready, now)
	if err != nil {
		return false, time.Time{}, nil, err
	}
	return true, time.Time{}, &batch, nil
}

func (s *Service) fetchLiveVehicles(ctx context.Context, catalog *model.Catalog, now time.Time) []model.LiveVehicle {
	if s.liveFetcher == nil {
		return nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, s.settings.LiveVehicleFetchTimeout)
	defer cancel()
	vehicles, err := s.liveFetcher(fetchCtx, catalog, now)
	if err != nil {
		log.Printf("satiksme chat analyzer live vehicle candidates unavailable: %v", err)
		return nil
	}
	return vehicles
}

func (s *Service) activeIncidents(ctx context.Context, catalog *model.Catalog, now time.Time) ([]model.IncidentSummary, error) {
	if s.incidents != nil {
		return s.incidents(ctx, catalog, now)
	}
	return s.reports.ListActiveIncidents(ctx, catalog, now, 0, 50)
}

type batchMessageOutcome struct {
	status           model.ChatAnalyzerMessageStatus
	analysisJSON     string
	appliedActionID  string
	appliedTargetKey string
	lastError        string
}

func (s *Service) processBatch(ctx context.Context, catalog *model.Catalog, vehicles []model.LiveVehicle, incidents []model.IncidentSummary, messages []model.ChatAnalyzerMessage, now time.Time) (model.ChatAnalyzerBatch, error) {
	if s.modelCircuitOpenUntil.After(now) {
		return model.ChatAnalyzerBatch{}, fmt.Errorf("model circuit is open until %s", s.modelCircuitOpenUntil.Format(time.RFC3339))
	}
	if len(messages) == 0 {
		return model.ChatAnalyzerBatch{}, nil
	}
	batchID := chatAnalyzerBatchID(now)
	batch := model.ChatAnalyzerBatch{
		ID:           batchID,
		Status:       model.ChatAnalyzerBatchRunning,
		DryRun:       s.settings.DryRun,
		StartedAt:    now,
		MessageCount: len(messages),
		Model:        s.modelName(),
	}
	if err := s.store.SaveChatAnalyzerBatch(ctx, batch); err != nil {
		return model.ChatAnalyzerBatch{}, fmt.Errorf("save chat analyzer batch start: %w", err)
	}

	stopDirectory := BuildStopDirectory(catalog)
	items := make([]BatchItem, 0, len(messages))
	for _, item := range messages {
		items = append(items, BatchItem{
			Message:       item,
			Candidates:    BuildCandidateContext(catalog, vehicles, incidents, item.Text),
			StopDirectory: stopDirectory,
		})
	}
	decision, raw, selectedModel, err := s.analyzer.AnalyzeBatch(ctx, items, incidents)
	if err != nil {
		s.recordModelFailure(now)
		batch.Status = model.ChatAnalyzerBatchFailed
		batch.FinishedAt = s.now().UTC()
		batch.Error = err.Error()
		batch.ErrorCount = len(messages)
		_ = s.store.SaveChatAnalyzerBatch(ctx, batch)
		for _, item := range messages {
			if markErr := s.mark(ctx, item.ID, model.ChatAnalyzerMessagePending, "", "", "", batchID, err.Error(), batch.FinishedAt); markErr != nil {
				log.Printf("satiksme chat analyzer mark failed after batch model error for %s: %v", item.ID, markErr)
			}
		}
		return batch, nil
	}
	s.resetModelFailures()
	batch.SelectedModel = strings.TrimSpace(selectedModel)
	if batch.SelectedModel == "" {
		batch.SelectedModel = strings.TrimSpace(decision.ModelMeta.SelectedModel)
	}
	batch.ReportCount = len(decision.Reports)
	batch.VoteCount = len(decision.Votes)
	batch.IgnoredCount = len(decision.Ignored)
	batch.ResultJSON = raw

	if reasoner, ok := s.analyzer.(LocationReasoningAnalyzer); ok {
		reasoningItems := items
		if freshVehicles := s.fetchLiveVehicles(ctx, catalog, s.now().UTC()); len(freshVehicles) > 0 {
			reasoningItems = rebuildBatchItemsWithVehicles(catalog, freshVehicles, incidents, items, stopDirectory)
		}
		reasoningItems, recheckIDs := locationReasoningItems(reasoningItems, decision)
		if len(recheckIDs) > 0 {
			reasoned, reasonedRaw, reasonedModel, reasonErr := reasoner.DeduceLocations(ctx, reasoningItems, incidents, decision, recheckIDs)
			if reasonErr != nil {
				log.Printf("satiksme chat analyzer location reasoning failed: %v", reasonErr)
			} else {
				decision = mergeLocationReasoningDecision(decision, reasoned, recheckIDs)
				items = reasoningItems
				if strings.TrimSpace(reasonedModel) != "" {
					batch.SelectedModel = strings.TrimSpace(reasonedModel)
				}
				batch.ReportCount = len(decision.Reports)
				batch.VoteCount = len(decision.Votes)
				batch.IgnoredCount = len(decision.Ignored)
				batch.ResultJSON = combinedBatchResultJSON(raw, reasonedRaw)
			}
		}
	}

	outcomes, stats := s.evaluateBatchDecisions(ctx, catalog, incidents, items, decision, batchID, now)
	batch.WouldApply = stats.wouldApply
	batch.AppliedCount = stats.applied
	batch.ErrorCount = stats.errors
	batch.Status = model.ChatAnalyzerBatchCompleted
	batch.FinishedAt = s.now().UTC()
	if err := s.store.SaveChatAnalyzerBatch(ctx, batch); err != nil {
		return model.ChatAnalyzerBatch{}, fmt.Errorf("save chat analyzer batch result: %w", err)
	}
	for _, item := range messages {
		outcome, ok := outcomes[item.MessageID]
		if !ok {
			outcome = batchMessageOutcome{
				status:       model.ChatAnalyzerMessageIgnored,
				analysisJSON: batchOutcomeJSON(batchID, "ignored", "model returned no decision", nil),
				lastError:    "model returned no decision",
			}
		}
		if err := s.mark(ctx, item.ID, outcome.status, outcome.analysisJSON, outcome.appliedActionID, outcome.appliedTargetKey, batchID, outcome.lastError, batch.FinishedAt); err != nil {
			return model.ChatAnalyzerBatch{}, fmt.Errorf("mark chat analyzer message %s: %w", item.ID, err)
		}
	}
	return batch, nil
}

func (s *Service) applyDecision(ctx context.Context, catalog *model.Catalog, item model.ChatAnalyzerMessage, decision Decision, target validatedTarget, now time.Time) (string, error) {
	userID, err := reporterUserID(item)
	if err != nil {
		return "", err
	}
	switch {
	case decision.TargetType == TargetStop && (decision.Action == ActionSighting || decision.Action == ActionNotice || decision.Action == ActionConfirmation):
		result, sighting, err := s.reports.SubmitStopSightingWithOptions(ctx, userID, target.stop.ID, now, reports.SubmitOptions{Source: model.IncidentVoteSourceTelegramChat})
		if err != nil {
			return "", err
		}
		if !result.Accepted {
			return "", reportResultError(result)
		}
		s.enqueueDumpForStop(target.stop, sighting)
		return sighting.ID, nil
	case decision.TargetType == TargetVehicle && (decision.Action == ActionSighting || decision.Action == ActionNotice || decision.Action == ActionConfirmation):
		result, sighting, err := s.reports.SubmitVehicleSightingWithOptions(ctx, userID, model.VehicleReportInput{
			Mode:             target.vehicle.Mode,
			RouteLabel:       target.vehicle.RouteLabel,
			Direction:        target.vehicle.Direction,
			Destination:      target.vehicle.Destination,
			DepartureSeconds: target.vehicle.DepartureSeconds,
			LiveRowID:        target.vehicle.LiveRowID,
		}, now, reports.SubmitOptions{Source: model.IncidentVoteSourceTelegramChat})
		if err != nil {
			return "", err
		}
		if !result.Accepted {
			return "", reportResultError(result)
		}
		s.enqueueDumpForVehicle(sighting)
		return sighting.ID, nil
	case decision.TargetType == TargetArea && (decision.Action == ActionSighting || decision.Action == ActionNotice || decision.Action == ActionConfirmation):
		result, areaReport, err := s.reports.SubmitAreaReportWithOptions(ctx, userID, model.AreaReportInput{
			Latitude:     target.area.Latitude,
			Longitude:    target.area.Longitude,
			RadiusMeters: areaRadiusForConfidence(target.area.RadiusMeters, decision.Confidence),
			Description:  areaReportDescription(target.area.Description, item.Text),
		}, now, reports.SubmitOptions{Source: model.IncidentVoteSourceTelegramChat})
		if err != nil {
			return "", err
		}
		if !result.Accepted {
			return "", reportResultError(result)
		}
		return areaReport.ID, nil
	case decision.Action == ActionConfirmation:
		_, err := s.reports.RecordIncidentVoteFromSource(ctx, catalog, target.incidentID, userID, model.IncidentVoteOngoing, model.IncidentVoteSourceTelegramChat, item.ID, now)
		return item.ID, err
	case decision.Action == ActionDenial || decision.Action == ActionCleared:
		_, err := s.reports.RecordIncidentVoteFromSource(ctx, catalog, target.incidentID, userID, model.IncidentVoteCleared, model.IncidentVoteSourceTelegramChat, item.ID, now)
		return item.ID, err
	default:
		return "", fmt.Errorf("unsupported validated action %q for target %q", decision.Action, decision.TargetType)
	}
}

func rebuildBatchItemsWithVehicles(catalog *model.Catalog, vehicles []model.LiveVehicle, incidents []model.IncidentSummary, items []BatchItem, stopDirectory []StopCandidate) []BatchItem {
	out := make([]BatchItem, 0, len(items))
	for _, item := range items {
		out = append(out, BatchItem{
			Message:       item.Message,
			Candidates:    BuildCandidateContext(catalog, vehicles, incidents, item.Message.Text),
			StopDirectory: stopDirectory,
		})
	}
	return out
}

func (s *Service) enqueueDumpForStop(stop StopCandidate, sighting *model.StopSighting) {
	if s == nil || s.dump == nil || sighting == nil || sighting.Hidden {
		return
	}
	s.dump.EnqueueStop(model.Stop{
		ID:          strings.TrimSpace(stop.ID),
		Name:        strings.TrimSpace(stop.Name),
		Modes:       append([]string(nil), stop.Modes...),
		RouteLabels: append([]string(nil), stop.RouteLabels...),
	}, sighting)
}

func (s *Service) enqueueDumpForVehicle(sighting *model.VehicleSighting) {
	if s == nil || s.dump == nil || sighting == nil || sighting.Hidden {
		return
	}
	s.dump.EnqueueVehicle(sighting)
}

func reportResultError(result model.ReportResult) error {
	switch {
	case result.Deduped:
		return fmt.Errorf("duplicate report")
	case result.RateLimited:
		return fmt.Errorf("rate limited: %s", result.Reason)
	default:
		return fmt.Errorf("report was not accepted")
	}
}

func areaRadiusForConfidence(base int, confidence float64) int {
	radius := base
	if radius <= 0 {
		radius = 250
	}
	if confidence < 0.88 && radius < 500 {
		radius = 500
	} else if confidence < 0.94 && radius < 250 {
		radius = 250
	}
	if radius > 500 {
		radius = 500
	}
	return radius
}

func areaReportDescription(text, fallback string) string {
	clean := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if clean == "" {
		clean = strings.Join(strings.Fields(strings.TrimSpace(fallback)), " ")
	}
	if clean == "" {
		return "approximate inspection area"
	}
	runes := []rune(clean)
	if len(runes) <= 160 {
		return clean
	}
	return string(runes[:157]) + "..."
}

const clearActionMinConfidence = 0.94

func confidenceThresholdForAction(action string, minConfidence float64) float64 {
	if minConfidence <= 0 {
		minConfidence = 0.82
	}
	if isClearAction(action) {
		if minConfidence > clearActionMinConfidence {
			return minConfidence
		}
		return clearActionMinConfidence
	}
	return minConfidence
}

func isClearAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case ActionCleared, ActionDenial:
		return true
	default:
		return false
	}
}

type batchDecisionStats struct {
	wouldApply int
	applied    int
	errors     int
}

type batchReportRef struct {
	incidentID string
	dedupeKey  string
}

func (s *Service) evaluateBatchDecisions(ctx context.Context, catalog *model.Catalog, incidents []model.IncidentSummary, items []BatchItem, decision BatchDecision, batchID string, now time.Time) (map[int64]batchMessageOutcome, batchDecisionStats) {
	outcomes := make(map[int64]batchMessageOutcome)
	stats := batchDecisionStats{}
	byMessageID := make(map[int64]BatchItem, len(items))
	for _, item := range items {
		byMessageID[item.Message.MessageID] = item
	}
	reportRefs := make(map[string]batchReportRef)
	for _, report := range decision.Reports {
		sources, candidates, err := batchSourcesAndCandidates(byMessageID, report.SourceMessageIDs)
		if err != nil {
			stats.errors++
			continue
		}
		normalized, err := normalizeDecision(Decision{
			Action:     report.Action,
			TargetType: report.TargetType,
			TargetID:   report.TargetID,
			Confidence: report.Confidence,
			Language:   report.Language,
			Reason:     report.Reason,
		})
		if err == nil && normalized.Action != ActionSighting && normalized.Action != ActionNotice && normalized.Action != ActionConfirmation {
			err = fmt.Errorf("report action %q is not publishable as an ongoing incident signal", normalized.Action)
		}
		if err == nil && normalized.Confidence < confidenceThresholdForAction(normalized.Action, s.settings.MinConfidence) {
			err = fmt.Errorf("low confidence")
		}
		var target validatedTarget
		if err == nil {
			target, err = validateTarget(normalized, candidates)
		}
		if err == nil && normalized.TargetType == TargetStop && ambiguousStopReportNeedsArea(sources[0].Message.Text, target.stop, candidates) {
			err = fmt.Errorf("ambiguous stop name should use area target")
		}
		if err == nil {
			_, err = reporterUserID(sources[0].Message)
		}
		if err == nil && target.dedupeKey != "" {
			var applied int
			applied, err = s.store.CountChatAnalyzerAppliedByTargetSince(ctx, target.dedupeKey, now.Add(-s.settings.TargetDedupeWindow))
			if err == nil && applied > 0 {
				err = fmt.Errorf("target duplicate window")
			}
		}
		analysis := batchOutcomeJSON(batchID, "report", "", report)
		if err != nil {
			stats.errors++
			markSources(outcomes, sources, batchMessageOutcome{
				status:       model.ChatAnalyzerMessageUncertain,
				analysisJSON: analysis,
				lastError:    err.Error(),
			})
			continue
		}
		if strings.TrimSpace(report.ID) != "" {
			reportRefs[strings.TrimSpace(report.ID)] = batchReportRef{incidentID: target.incidentID, dedupeKey: target.dedupeKey}
		}
		status := model.ChatAnalyzerMessageDryRun
		actionID := ""
		if s.settings.DryRun {
			stats.wouldApply++
		} else {
			var applyErr error
			actionID, applyErr = s.applyDecision(ctx, catalog, sources[0].Message, normalized, target, now)
			if applyErr != nil {
				stats.errors++
				markSources(outcomes, sources, batchMessageOutcome{
					status:           model.ChatAnalyzerMessageUncertain,
					analysisJSON:     analysis,
					appliedTargetKey: target.dedupeKey,
					lastError:        applyErr.Error(),
				})
				continue
			}
			status = model.ChatAnalyzerMessageApplied
			stats.applied++
		}
		markSources(outcomes, sources, batchMessageOutcome{
			status:           status,
			analysisJSON:     analysis,
			appliedActionID:  actionID,
			appliedTargetKey: target.dedupeKey,
		})
	}
	for _, vote := range decision.Votes {
		sources, candidates, err := batchSourcesAndCandidates(byMessageID, vote.SourceMessageIDs)
		if err != nil {
			stats.errors++
			continue
		}
		normalized := Decision{
			Action:     strings.ToLower(strings.TrimSpace(vote.Action)),
			TargetType: strings.ToLower(strings.TrimSpace(vote.TargetType)),
			TargetID:   strings.TrimSpace(vote.TargetID),
			Confidence: vote.Confidence,
			Language:   vote.Language,
			Reason:     vote.Reason,
		}
		if normalized.Action != ActionConfirmation && normalized.Action != ActionDenial && normalized.Action != ActionCleared {
			stats.errors++
			err := fmt.Errorf("unsupported vote action %q", normalized.Action)
			markSources(outcomes, sources, batchMessageOutcome{
				status:       model.ChatAnalyzerMessageUncertain,
				analysisJSON: batchOutcomeJSON(batchID, "vote", "", vote),
				lastError:    err.Error(),
			})
			continue
		}
		if normalized.Confidence < confidenceThresholdForAction(normalized.Action, s.settings.MinConfidence) {
			stats.errors++
			markSources(outcomes, sources, batchMessageOutcome{
				status:       model.ChatAnalyzerMessageUncertain,
				analysisJSON: batchOutcomeJSON(batchID, "vote", "", vote),
				lastError:    "low confidence",
			})
			continue
		}
		var target validatedTarget
		switch normalized.TargetType {
		case TargetIncident:
			target, err = validateActiveIncident(normalized.TargetID, incidents, normalized.Action)
		case "report":
			ref, ok := reportRefs[normalized.TargetID]
			if !ok {
				err = fmt.Errorf("referenced report was not validated")
			} else {
				target = validatedTarget{incidentID: ref.incidentID, dedupeKey: ongoingVoteDedupeKey(ref.incidentID)}
			}
		default:
			normalized.TargetType = TargetIncident
			target, err = validateTarget(normalized, candidates)
		}
		analysis := batchOutcomeJSON(batchID, "vote", "", vote)
		if err == nil {
			_, err = reporterUserID(sources[0].Message)
		}
		if err == nil && target.dedupeKey != "" {
			var applied int
			applied, err = s.store.CountChatAnalyzerAppliedByTargetSince(ctx, target.dedupeKey, now.Add(-s.settings.TargetDedupeWindow))
			if err == nil && applied > 0 {
				err = fmt.Errorf("target duplicate window")
			}
		}
		if err != nil {
			stats.errors++
			markSources(outcomes, sources, batchMessageOutcome{
				status:       model.ChatAnalyzerMessageUncertain,
				analysisJSON: analysis,
				lastError:    err.Error(),
			})
			continue
		}
		status := model.ChatAnalyzerMessageDryRun
		actionID := ""
		if s.settings.DryRun {
			stats.wouldApply++
		} else {
			normalized.TargetType = TargetIncident
			normalized.TargetID = target.incidentID
			var applyErr error
			actionID, applyErr = s.applyDecision(ctx, catalog, sources[0].Message, normalized, target, now)
			if applyErr != nil {
				stats.errors++
				markSources(outcomes, sources, batchMessageOutcome{
					status:           model.ChatAnalyzerMessageUncertain,
					analysisJSON:     analysis,
					appliedTargetKey: target.dedupeKey,
					lastError:        applyErr.Error(),
				})
				continue
			}
			status = model.ChatAnalyzerMessageApplied
			stats.applied++
		}
		markSources(outcomes, sources, batchMessageOutcome{
			status:           status,
			analysisJSON:     analysis,
			appliedActionID:  actionID,
			appliedTargetKey: target.dedupeKey,
		})
	}
	for _, ignored := range decision.Ignored {
		if _, exists := outcomes[ignored.MessageID]; exists {
			continue
		}
		if _, ok := byMessageID[ignored.MessageID]; !ok {
			continue
		}
		outcomes[ignored.MessageID] = batchMessageOutcome{
			status:       model.ChatAnalyzerMessageIgnored,
			analysisJSON: batchOutcomeJSON(batchID, "ignored", ignored.Reason, ignored),
			lastError:    strings.TrimSpace(ignored.Reason),
		}
	}
	return outcomes, stats
}

func locationReasoningItems(items []BatchItem, decision BatchDecision) ([]BatchItem, []int64) {
	if len(items) == 0 {
		return nil, nil
	}
	recheck := locationReasoningMessageIDs(items, decision)
	if len(recheck) == 0 {
		return items, nil
	}
	out := append([]BatchItem(nil), items...)
	for i := range out {
		if _, ok := recheck[out[i].Message.MessageID]; !ok {
			continue
		}
		out[i].Candidates = locationReasoningCandidates(out, i)
	}
	ids := make([]int64, 0, len(recheck))
	for _, item := range out {
		if _, ok := recheck[item.Message.MessageID]; ok {
			ids = append(ids, item.Message.MessageID)
		}
	}
	return out, ids
}

func locationReasoningMessageIDs(items []BatchItem, decision BatchDecision) map[int64]struct{} {
	decided := make(map[int64]struct{})
	for _, report := range decision.Reports {
		for _, id := range report.SourceMessageIDs {
			if shouldRecheckReportLocation(items, id, report) {
				continue
			}
			decided[id] = struct{}{}
		}
	}
	for _, vote := range decision.Votes {
		for _, id := range vote.SourceMessageIDs {
			decided[id] = struct{}{}
		}
	}
	recheck := make(map[int64]struct{})
	for _, ignored := range decision.Ignored {
		reason := strings.ToLower(strings.TrimSpace(ignored.Reason))
		if strings.Contains(reason, "vague") ||
			strings.Contains(reason, "location") ||
			strings.Contains(reason, "ambiguous") ||
			strings.Contains(reason, "unclear") ||
			strings.Contains(reason, "target") ||
			strings.Contains(reason, "place") {
			recheck[ignored.MessageID] = struct{}{}
		}
		decided[ignored.MessageID] = struct{}{}
	}
	for _, item := range items {
		if _, ok := decided[item.Message.MessageID]; ok {
			continue
		}
		if looksLikeTransportSignal(item.Message.Text) {
			recheck[item.Message.MessageID] = struct{}{}
		}
	}
	return recheck
}

func shouldRecheckReportLocation(items []BatchItem, messageID int64, report BatchReportDecision) bool {
	targetType := strings.ToLower(strings.TrimSpace(report.TargetType))
	if targetType == TargetArea {
		return false
	}
	if report.Confidence > 0 && report.Confidence < 0.88 {
		return true
	}
	reason := strings.ToLower(strings.TrimSpace(report.Reason))
	if strings.Contains(reason, "vague") ||
		strings.Contains(reason, "approx") ||
		strings.Contains(reason, "between") ||
		strings.Contains(reason, "ambiguous") ||
		strings.Contains(reason, "unclear") {
		return true
	}
	for _, item := range items {
		if item.Message.MessageID != messageID {
			continue
		}
		if targetType != TargetStop {
			return false
		}
		stop, ok := findStopCandidate(report.TargetID, item.Candidates.Stops)
		if !ok {
			return false
		}
		return (looksLikeApproximateAreaText(item.Message.Text) && len(item.Candidates.Areas) > 0) ||
			ambiguousStopReportNeedsArea(item.Message.Text, stop, item.Candidates)
	}
	return false
}

func ambiguousStopReportNeedsArea(text string, stop StopCandidate, candidates CandidateContext) bool {
	group := sameNameStopCandidates(stop, candidates.Stops)
	if len(group) < 2 {
		return false
	}
	if !hasSameNameAreaCandidate(stop, candidates.Areas) {
		return false
	}
	return !stopReportDisambiguatedByText(text, stop, group)
}

func findStopCandidate(id string, stops []StopCandidate) (StopCandidate, bool) {
	clean := strings.TrimSpace(id)
	for _, stop := range stops {
		if strings.TrimSpace(stop.ID) == clean {
			return stop, true
		}
	}
	return StopCandidate{}, false
}

func sameNameStopCandidates(stop StopCandidate, stops []StopCandidate) []StopCandidate {
	key := stopNameKey(stop.Name)
	if key == "" {
		return nil
	}
	out := make([]StopCandidate, 0, len(stops))
	for _, candidate := range stops {
		if stopNameKey(candidate.Name) == key {
			out = append(out, candidate)
		}
	}
	return out
}

func hasSameNameAreaCandidate(stop StopCandidate, areas []AreaCandidate) bool {
	id := "name:" + strings.ReplaceAll(stopNameKey(stop.Name), " ", "-")
	for _, area := range areas {
		if strings.TrimSpace(area.ID) == id {
			return true
		}
	}
	return false
}

func stopReportDisambiguatedByText(text string, stop StopCandidate, group []StopCandidate) bool {
	normalized := normalizeText(text)
	routes := routeLabelsFromText(normalized)
	for route := range routes {
		matches := 0
		selected := false
		for _, candidate := range group {
			if stopHasRoute(candidate, route) {
				matches++
				if candidate.ID == stop.ID {
					selected = true
				}
			}
		}
		if matches == 1 && selected {
			return true
		}
	}
	for mode := range mentionedModes(normalized) {
		matches := 0
		selected := false
		for _, candidate := range group {
			if stopHasMode(candidate, mode) {
				matches++
				if candidate.ID == stop.ID {
					selected = true
				}
			}
		}
		if matches == 1 && selected {
			return true
		}
	}
	return false
}

func stopHasRoute(stop StopCandidate, route string) bool {
	for _, label := range stop.RouteLabels {
		if normalizeRouteLabel(label) == route {
			return true
		}
	}
	return false
}

func mentionedModes(normalizedText string) map[string]struct{} {
	out := make(map[string]struct{})
	if strings.Contains(normalizedText, "tram") || strings.Contains(normalizedText, "tramv") {
		out["tram"] = struct{}{}
	}
	if strings.Contains(normalizedText, "trol") {
		out["trol"] = struct{}{}
	}
	if strings.Contains(normalizedText, "bus") || strings.Contains(normalizedText, "autobus") || strings.Contains(normalizedText, "avtobus") {
		out["bus"] = struct{}{}
	}
	return out
}

func stopHasMode(stop StopCandidate, mode string) bool {
	for _, candidate := range stop.Modes {
		if strings.Contains(strings.ToLower(strings.TrimSpace(candidate)), mode) {
			return true
		}
	}
	return false
}

func locationReasoningCandidates(items []BatchItem, index int) CandidateContext {
	merged := copyCandidateContext(items[index].Candidates)
	for i := range items {
		if i == index {
			continue
		}
		if !nearbyForLocationReasoning(items[index].Message, items[i].Message) {
			continue
		}
		merged.Stops = append(merged.Stops, items[i].Candidates.Stops...)
		merged.Vehicles = append(merged.Vehicles, items[i].Candidates.Vehicles...)
		merged.Areas = append(merged.Areas, items[i].Candidates.Areas...)
		merged.Incidents = append(merged.Incidents, items[i].Candidates.Incidents...)
	}
	merged = dedupeCandidates(merged)
	if len(merged.Stops) > maxStopCandidates+4 {
		merged.Stops = merged.Stops[:maxStopCandidates+4]
	}
	if len(merged.Vehicles) > maxVehicleCandidates+4 {
		merged.Vehicles = merged.Vehicles[:maxVehicleCandidates+4]
	}
	if len(merged.Areas) > maxAreaCandidates+4 {
		merged.Areas = merged.Areas[:maxAreaCandidates+4]
	}
	if len(merged.Incidents) > maxIncidentCandidates+4 {
		merged.Incidents = merged.Incidents[:maxIncidentCandidates+4]
	}
	return merged
}

func copyCandidateContext(candidates CandidateContext) CandidateContext {
	return CandidateContext{
		Stops:     append([]StopCandidate(nil), candidates.Stops...),
		Vehicles:  append([]VehicleCandidate(nil), candidates.Vehicles...),
		Areas:     append([]AreaCandidate(nil), candidates.Areas...),
		Incidents: append([]IncidentCandidate(nil), candidates.Incidents...),
	}
}

func nearbyForLocationReasoning(target, context model.ChatAnalyzerMessage) bool {
	if target.ReplyToMessageID != 0 && target.ReplyToMessageID == context.MessageID {
		return true
	}
	if context.ReplyToMessageID != 0 && context.ReplyToMessageID == target.MessageID {
		return true
	}
	if !target.MessageDate.IsZero() && !context.MessageDate.IsZero() {
		delta := target.MessageDate.Sub(context.MessageDate)
		if delta < 0 {
			delta = -delta
		}
		return delta <= 15*time.Minute
	}
	delta := target.MessageID - context.MessageID
	if delta < 0 {
		delta = -delta
	}
	return delta <= 5
}

func looksLikeTransportSignal(text string) bool {
	clean := normalizeText(text)
	if clean == "" {
		return false
	}
	needles := []string{
		"kontrole", "kontrol", "controller", "inspection", "ticket", "parbaude", "sods",
		"menti", "policija", "municipal", "rpp", "iekapa", "izkapa", "stav", "brauc",
		"есть", "контрол", "провер", "штраф", "полици",
		"зашли", "зашел", "сели", "сел", "вошли", "вошел", "вышли", "вышел", "едут", "ехали", "стоят", "стоит",
	}
	for _, needle := range needles {
		if strings.Contains(clean, normalizeText(needle)) {
			return true
		}
	}
	return false
}

func mergeLocationReasoningDecision(initial BatchDecision, reasoned BatchDecision, recheckMessageIDs []int64) BatchDecision {
	recheck := make(map[int64]struct{}, len(recheckMessageIDs))
	for _, id := range recheckMessageIDs {
		recheck[id] = struct{}{}
	}
	reasonedIDs := make(map[int64]struct{})
	reasonedReports := make([]BatchReportDecision, 0, len(reasoned.Reports))
	reasonedVotes := make([]BatchVoteDecision, 0, len(reasoned.Votes))
	reasonedIgnored := make(map[int64]BatchIgnoredDecision)
	for _, report := range reasoned.Reports {
		report.SourceMessageIDs = onlyRecheckSourceIDs(report.SourceMessageIDs, recheck)
		if len(report.SourceMessageIDs) == 0 {
			continue
		}
		report.Reason = locationReasoningReason(report.Reason)
		reasonedReports = append(reasonedReports, report)
		for _, id := range report.SourceMessageIDs {
			reasonedIDs[id] = struct{}{}
		}
	}
	for _, vote := range reasoned.Votes {
		vote.SourceMessageIDs = onlyRecheckSourceIDs(vote.SourceMessageIDs, recheck)
		if len(vote.SourceMessageIDs) == 0 {
			continue
		}
		vote.Reason = locationReasoningReason(vote.Reason)
		reasonedVotes = append(reasonedVotes, vote)
		for _, id := range vote.SourceMessageIDs {
			reasonedIDs[id] = struct{}{}
		}
	}
	for _, item := range reasoned.Ignored {
		if _, ok := recheck[item.MessageID]; !ok {
			continue
		}
		item.Reason = locationReasoningReason(item.Reason)
		reasonedIgnored[item.MessageID] = item
		reasonedIDs[item.MessageID] = struct{}{}
	}
	out := BatchDecision{ModelMeta: initial.ModelMeta}
	for _, report := range initial.Reports {
		if anySourceIDIn(report.SourceMessageIDs, reasonedIDs) {
			continue
		}
		out.Reports = append(out.Reports, report)
	}
	for _, vote := range initial.Votes {
		if anySourceIDIn(vote.SourceMessageIDs, reasonedIDs) {
			continue
		}
		out.Votes = append(out.Votes, vote)
	}
	ignored := make([]BatchIgnoredDecision, 0, len(initial.Ignored)+len(reasoned.Ignored))
	for _, item := range initial.Ignored {
		if _, ok := reasonedIDs[item.MessageID]; ok {
			continue
		}
		if next, ok := reasonedIgnored[item.MessageID]; ok {
			ignored = append(ignored, next)
			delete(reasonedIgnored, item.MessageID)
			continue
		}
		ignored = append(ignored, item)
	}
	for _, item := range reasonedIgnored {
		ignored = append(ignored, item)
	}
	out.Reports = append(out.Reports, reasonedReports...)
	out.Votes = append(out.Votes, reasonedVotes...)
	out.Ignored = ignored
	return out
}

func anySourceIDIn(ids []int64, targets map[int64]struct{}) bool {
	for _, id := range ids {
		if _, ok := targets[id]; ok {
			return true
		}
	}
	return false
}

func onlyRecheckSourceIDs(ids []int64, recheck map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := recheck[id]; !ok {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func locationReasoningReason(reason string) string {
	clean := strings.TrimSpace(reason)
	if clean == "" {
		return "location deduction"
	}
	if strings.Contains(strings.ToLower(clean), "deduc") {
		return clean
	}
	return "location deduction: " + clean
}

func combinedBatchResultJSON(initialRaw, reasoningRaw string) string {
	body, err := json.Marshal(struct {
		Initial           json.RawMessage `json:"initial,omitempty"`
		LocationReasoning json.RawMessage `json:"locationReasoning,omitempty"`
	}{
		Initial:           rawJSONOrString(initialRaw),
		LocationReasoning: rawJSONOrString(reasoningRaw),
	})
	if err != nil {
		return initialRaw
	}
	return string(body)
}

func rawJSONOrString(raw string) json.RawMessage {
	clean := strings.TrimSpace(raw)
	if clean == "" {
		return nil
	}
	if json.Valid([]byte(clean)) {
		return json.RawMessage(clean)
	}
	body, err := json.Marshal(clean)
	if err != nil {
		return nil
	}
	return json.RawMessage(body)
}

func reporterUserID(item model.ChatAnalyzerMessage) (int64, error) {
	if userID, ok := model.ChatAnalyzerReporterUserID(item.SenderID); ok {
		return userID, nil
	}
	return 0, fmt.Errorf("telegram user id is required")
}

func batchSourcesAndCandidates(byMessageID map[int64]BatchItem, sourceIDs []int64) ([]BatchItem, CandidateContext, error) {
	if len(sourceIDs) == 0 {
		return nil, CandidateContext{}, fmt.Errorf("sourceMessageIds is required")
	}
	sources := make([]BatchItem, 0, len(sourceIDs))
	seen := make(map[int64]struct{}, len(sourceIDs))
	var candidates CandidateContext
	for _, messageID := range sourceIDs {
		if _, ok := seen[messageID]; ok {
			continue
		}
		item, ok := byMessageID[messageID]
		if !ok {
			return nil, CandidateContext{}, fmt.Errorf("source message %d was not in the batch", messageID)
		}
		seen[messageID] = struct{}{}
		sources = append(sources, item)
		candidates.Stops = append(candidates.Stops, item.Candidates.Stops...)
		candidates.Vehicles = append(candidates.Vehicles, item.Candidates.Vehicles...)
		candidates.Areas = append(candidates.Areas, item.Candidates.Areas...)
		candidates.Incidents = append(candidates.Incidents, item.Candidates.Incidents...)
	}
	return sources, dedupeCandidates(candidates), nil
}

func dedupeCandidates(candidates CandidateContext) CandidateContext {
	stopSeen := make(map[string]struct{}, len(candidates.Stops))
	stops := candidates.Stops[:0]
	for _, item := range candidates.Stops {
		if _, ok := stopSeen[item.ID]; ok {
			continue
		}
		stopSeen[item.ID] = struct{}{}
		stops = append(stops, item)
	}
	vehicleSeen := make(map[string]struct{}, len(candidates.Vehicles))
	vehicles := candidates.Vehicles[:0]
	for _, item := range candidates.Vehicles {
		if _, ok := vehicleSeen[item.ID]; ok {
			continue
		}
		vehicleSeen[item.ID] = struct{}{}
		vehicles = append(vehicles, item)
	}
	areaSeen := make(map[string]struct{}, len(candidates.Areas))
	areas := candidates.Areas[:0]
	for _, item := range candidates.Areas {
		if _, ok := areaSeen[item.ID]; ok {
			continue
		}
		areaSeen[item.ID] = struct{}{}
		areas = append(areas, item)
	}
	incidentSeen := make(map[string]struct{}, len(candidates.Incidents))
	incidents := candidates.Incidents[:0]
	for _, item := range candidates.Incidents {
		if _, ok := incidentSeen[item.ID]; ok {
			continue
		}
		incidentSeen[item.ID] = struct{}{}
		incidents = append(incidents, item)
	}
	return CandidateContext{Stops: stops, Vehicles: vehicles, Areas: areas, Incidents: incidents}
}

func validateActiveIncident(incidentID string, incidents []model.IncidentSummary, action string) (validatedTarget, error) {
	clean := strings.TrimSpace(incidentID)
	for _, incident := range incidents {
		if strings.TrimSpace(incident.ID) == clean {
			cleanAction := strings.ToLower(strings.TrimSpace(action))
			dedupeKey := "vote:" + clean + ":" + cleanAction
			if cleanAction == ActionConfirmation {
				dedupeKey = ongoingVoteDedupeKey(clean)
			}
			return validatedTarget{incidentID: clean, dedupeKey: dedupeKey}, nil
		}
	}
	return validatedTarget{}, fmt.Errorf("incident was not active")
}

func ongoingVoteDedupeKey(incidentID string) string {
	return "vote:" + strings.TrimSpace(incidentID) + ":" + ActionSighting
}

func markSources(outcomes map[int64]batchMessageOutcome, sources []BatchItem, outcome batchMessageOutcome) {
	for _, source := range sources {
		outcomes[source.Message.MessageID] = outcome
	}
}

func batchOutcomeJSON(batchID, kind, note string, payload any) string {
	body, err := json.Marshal(struct {
		BatchID string `json:"batchId"`
		Kind    string `json:"kind"`
		Note    string `json:"note,omitempty"`
		Payload any    `json:"payload,omitempty"`
	}{
		BatchID: batchID,
		Kind:    strings.TrimSpace(kind),
		Note:    strings.TrimSpace(note),
		Payload: payload,
	})
	if err != nil {
		return ""
	}
	return string(body)
}

func chatAnalyzerBatchID(now time.Time) string {
	return fmt.Sprintf("chat-batch-%s-%d", now.UTC().Format("20060102T150405Z"), now.UnixNano())
}

func (s *Service) modelName() string {
	return strings.TrimSpace(s.settings.ModelName)
}

func (s *Service) mark(ctx context.Context, id string, status model.ChatAnalyzerMessageStatus, analysisJSON, appliedActionID, appliedTargetKey, batchID, lastError string, processedAt time.Time) error {
	return s.store.MarkChatAnalyzerMessageProcessedInBatch(ctx, id, status, analysisJSON, appliedActionID, appliedTargetKey, batchID, lastError, processedAt)
}

func (s *Service) recordModelFailure(now time.Time) {
	s.consecutiveModelFailures++
	if s.consecutiveModelFailures >= s.settings.ModelFailureLimit {
		s.modelCircuitOpenUntil = now.Add(s.settings.ModelCircuitOpen)
		log.Printf("satiksme chat analyzer model circuit open until %s after %d failures", s.modelCircuitOpenUntil.Format(time.RFC3339), s.consecutiveModelFailures)
	}
}

func (s *Service) resetModelFailures() {
	s.consecutiveModelFailures = 0
	s.modelCircuitOpenUntil = time.Time{}
}

func (s *Service) nextDelay(now, nextCollect, nextProcess, nextRetry time.Time) time.Duration {
	nextWake := nextCollect
	if !nextProcess.IsZero() && (nextWake.IsZero() || nextProcess.Before(nextWake)) {
		nextWake = nextProcess
	}
	if !nextRetry.IsZero() && (nextWake.IsZero() || nextRetry.Before(nextWake)) {
		nextWake = nextRetry
	}
	delay := nextWake.Sub(now)
	if delay <= 0 {
		return time.Second
	}
	if delay > s.settings.PollInterval {
		return s.settings.PollInterval
	}
	return delay
}

func (s *Service) throttledProcessAt(candidate, lastProcessAt time.Time) time.Time {
	if candidate.IsZero() {
		return time.Time{}
	}
	next := candidate.UTC()
	if !lastProcessAt.IsZero() {
		minimum := lastProcessAt.UTC().Add(s.settings.ProcessInterval)
		if next.Before(minimum) {
			next = minimum
		}
	}
	window := nextProcessWindowOpenAt(next, s.settings)
	if next.Before(window) {
		return window
	}
	return next
}

func (s *Service) messageReadyForRetry(item model.ChatAnalyzerMessage, now time.Time) bool {
	if item.Attempts <= 0 || item.ProcessedAt.IsZero() {
		return true
	}
	return !now.Before(item.ProcessedAt.Add(s.retryDelay(item.Attempts)))
}

func (s *Service) retryDelay(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	delay := s.settings.RetryBaseDelay
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= s.settings.RetryMaxDelay {
			return s.settings.RetryMaxDelay
		}
	}
	if delay > s.settings.RetryMaxDelay {
		return s.settings.RetryMaxDelay
	}
	return delay
}

func nextScheduledProcessAt(now time.Time, settings Settings) time.Time {
	local := now.In(settings.Location)
	start := localMidnight(local).Add(time.Duration(settings.ProcessStartMinute) * time.Minute)
	end := localMidnight(local).Add(time.Duration(settings.ProcessEndMinute) * time.Minute)

	if settings.ProcessEndMinute > settings.ProcessStartMinute {
		if local.Before(start) {
			return start.In(time.UTC)
		}
		if candidate, ok := scheduledProcessCandidate(local, start, end, settings.ProcessInterval); ok {
			return candidate.In(time.UTC)
		}
		return start.AddDate(0, 0, 1).In(time.UTC)
	}

	if !local.After(end) {
		previousStart := start.AddDate(0, 0, -1)
		if candidate, ok := scheduledProcessCandidate(local, previousStart, end, settings.ProcessInterval); ok {
			return candidate.In(time.UTC)
		}
	}
	if local.Before(start) {
		return start.In(time.UTC)
	}
	nextEnd := end.AddDate(0, 0, 1)
	if candidate, ok := scheduledProcessCandidate(local, start, nextEnd, settings.ProcessInterval); ok {
		return candidate.In(time.UTC)
	}
	return start.AddDate(0, 0, 1).In(time.UTC)
}

func nextScheduledProcessAfter(now time.Time, settings Settings) time.Time {
	return nextScheduledProcessAt(now.Add(time.Second), settings)
}

func nextProcessWindowOpenAt(now time.Time, settings Settings) time.Time {
	local := now.In(settings.Location)
	start := localMidnight(local).Add(time.Duration(settings.ProcessStartMinute) * time.Minute)
	end := localMidnight(local).Add(time.Duration(settings.ProcessEndMinute) * time.Minute)

	if settings.ProcessEndMinute > settings.ProcessStartMinute {
		if local.Before(start) {
			return start.In(time.UTC)
		}
		if !local.After(end) {
			return now.UTC()
		}
		return start.AddDate(0, 0, 1).In(time.UTC)
	}

	if !local.After(end) {
		return now.UTC()
	}
	if local.Before(start) {
		return start.In(time.UTC)
	}
	return now.UTC()
}

func scheduledProcessCandidate(local, start, end time.Time, interval time.Duration) (time.Time, bool) {
	if local.Before(start) || local.After(end) {
		return time.Time{}, false
	}
	elapsed := local.Sub(start)
	slots := int64(elapsed / interval)
	candidate := start.Add(time.Duration(slots) * interval)
	if candidate.Before(local) {
		candidate = candidate.Add(interval)
	}
	if candidate.After(end) {
		return time.Time{}, false
	}
	return candidate, true
}

func localMidnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

type validatedTarget struct {
	stop       StopCandidate
	vehicle    VehicleCandidate
	area       AreaCandidate
	incidentID string
	dedupeKey  string
}

func normalizeDecision(decision Decision) (Decision, error) {
	decision.Action = strings.ToLower(strings.TrimSpace(decision.Action))
	decision.TargetType = strings.ToLower(strings.TrimSpace(decision.TargetType))
	decision.TargetID = strings.TrimSpace(decision.TargetID)
	switch decision.Action {
	case ActionSighting, ActionNotice, ActionCleared, ActionConfirmation, ActionDenial, ActionIgnore:
	default:
		return Decision{}, fmt.Errorf("unsupported action %q", decision.Action)
	}
	if decision.Action == ActionIgnore {
		decision.TargetType = TargetNone
		return decision, nil
	}
	switch decision.TargetType {
	case TargetStop, TargetVehicle, TargetArea, TargetIncident:
		if decision.TargetID == "" {
			return Decision{}, fmt.Errorf("missing target id")
		}
	case TargetNone, "":
		return Decision{}, fmt.Errorf("missing target type")
	default:
		return Decision{}, fmt.Errorf("unsupported target type %q", decision.TargetType)
	}
	return decision, nil
}

func validateTarget(decision Decision, candidates CandidateContext) (validatedTarget, error) {
	switch decision.TargetType {
	case TargetStop:
		for _, stop := range candidates.Stops {
			if stop.ID == decision.TargetID {
				incidentID := reports.StopIncidentID(stop.ID)
				if decision.Action == ActionDenial || decision.Action == ActionCleared {
					return validatedTarget{stop: stop, incidentID: incidentID, dedupeKey: "vote:" + incidentID + ":" + decision.Action}, nil
				}
				return validatedTarget{stop: stop, incidentID: incidentID, dedupeKey: "sighting:stop:" + stop.ID}, nil
			}
		}
	case TargetVehicle:
		for _, vehicle := range candidates.Vehicles {
			if vehicle.ID == decision.TargetID {
				incidentID := reports.VehicleIncidentID(vehicle.ID)
				if decision.Action == ActionDenial || decision.Action == ActionCleared {
					return validatedTarget{vehicle: vehicle, incidentID: incidentID, dedupeKey: "vote:" + incidentID + ":" + decision.Action}, nil
				}
				return validatedTarget{vehicle: vehicle, incidentID: incidentID, dedupeKey: "sighting:vehicle:" + vehicle.ID}, nil
			}
		}
	case TargetArea:
		for _, area := range candidates.Areas {
			if area.ID == decision.TargetID {
				area.RadiusMeters = areaRadiusForConfidence(area.RadiusMeters, decision.Confidence)
				scopeKey := reports.AreaScopeKey(model.AreaReportInput{
					Latitude:     area.Latitude,
					Longitude:    area.Longitude,
					RadiusMeters: area.RadiusMeters,
					Description:  area.Description,
				})
				incidentID := reports.AreaIncidentID(scopeKey)
				if decision.Action == ActionDenial || decision.Action == ActionCleared {
					return validatedTarget{area: area, incidentID: incidentID, dedupeKey: "vote:" + incidentID + ":" + decision.Action}, nil
				}
				return validatedTarget{area: area, incidentID: incidentID, dedupeKey: "sighting:area:" + scopeKey}, nil
			}
		}
	case TargetIncident:
		for _, incident := range candidates.Incidents {
			if incident.ID == decision.TargetID {
				return validatedTarget{incidentID: incident.ID, dedupeKey: "vote:" + incident.ID + ":" + decision.Action}, nil
			}
		}
	}
	return validatedTarget{}, fmt.Errorf("target was not in validated candidates")
}

func decisionJSON(decision Decision) string {
	body, err := json.Marshal(decision)
	if err != nil {
		return ""
	}
	return string(body)
}
