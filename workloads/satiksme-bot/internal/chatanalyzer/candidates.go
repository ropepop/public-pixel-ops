package chatanalyzer

import (
	"sort"
	"strings"
	"unicode"

	"satiksmebot/internal/model"
)

const (
	maxStopCandidates     = 8
	maxVehicleCandidates  = 8
	maxIncidentCandidates = 8
)

func BuildCandidateContext(catalog *model.Catalog, vehicles []model.LiveVehicle, incidents []model.IncidentSummary, text string) CandidateContext {
	return CandidateContext{
		Stops:     candidateStops(catalog, text),
		Vehicles:  candidateVehicles(vehicles, text),
		Incidents: candidateIncidents(incidents, text),
	}
}

func candidateStops(catalog *model.Catalog, text string) []StopCandidate {
	if catalog == nil {
		return nil
	}
	needle := normalizeText(text)
	type scored struct {
		score int
		item  StopCandidate
	}
	items := make([]scored, 0, len(catalog.Stops))
	for _, stop := range catalog.Stops {
		candidateParts := []string{stop.ID, stop.LiveID, stop.Name}
		candidateParts = append(candidateParts, stop.RouteLabels...)
		candidateParts = append(candidateParts, stopLocationAliases(stop.Name)...)
		candidateText := strings.Join(candidateParts, " ")
		score := fuzzyScore(needle, normalizeText(candidateText))
		if score <= 0 && len(catalog.Stops) > maxStopCandidates {
			continue
		}
		items = append(items, scored{
			score: score,
			item: StopCandidate{
				ID:          strings.TrimSpace(stop.ID),
				Name:        strings.TrimSpace(stop.Name),
				Modes:       append([]string(nil), stop.Modes...),
				RouteLabels: append([]string(nil), stop.RouteLabels...),
				Score:       score,
			},
		})
	}
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].score == items[right].score {
			return items[left].item.Name < items[right].item.Name
		}
		return items[left].score > items[right].score
	})
	if len(items) > maxStopCandidates {
		items = items[:maxStopCandidates]
	}
	out := make([]StopCandidate, 0, len(items))
	for _, item := range items {
		out = append(out, item.item)
	}
	return out
}

func stopLocationAliases(name string) []string {
	normalized := normalizeText(name)
	switch {
	case strings.Contains(normalized, "centraltirgus"):
		return []string{
			"central market", "centralais tirgus", "tirgus", "рынок", "центральный рынок", "центральном рынке", "centralny rynok", "pod mostom", "под мостом",
		}
	case strings.Contains(normalized, "centrala stacija"):
		return []string{
			"central station", "railway station", "train station", "vokzal", "вокзал", "центральная станция", "центральный вокзал",
		}
	case strings.Contains(normalized, "autoosta"):
		return []string{
			"bus station", "bus terminal", "автовокзал", "автостанция",
		}
	}
	return nil
}

func candidateVehicles(vehicles []model.LiveVehicle, text string) []VehicleCandidate {
	needle := normalizeText(text)
	type scored struct {
		score int
		item  VehicleCandidate
	}
	items := make([]scored, 0, len(vehicles))
	for _, vehicle := range vehicles {
		scopeID := vehicleScopeCandidateID(vehicle)
		candidateText := strings.Join([]string{
			scopeID,
			vehicle.ID,
			vehicle.VehicleCode,
			vehicle.Mode,
			vehicle.RouteLabel,
			vehicle.Direction,
			vehicle.Destination,
			vehicle.StopID,
			vehicle.StopName,
			vehicle.LiveRowID,
		}, " ")
		score := fuzzyScore(needle, normalizeText(candidateText))
		if score <= 0 && len(vehicles) > maxVehicleCandidates {
			continue
		}
		items = append(items, scored{
			score: score,
			item: VehicleCandidate{
				ID:               scopeID,
				Mode:             strings.TrimSpace(vehicle.Mode),
				RouteLabel:       strings.TrimSpace(vehicle.RouteLabel),
				Direction:        strings.TrimSpace(vehicle.Direction),
				Destination:      strings.TrimSpace(vehicle.Destination),
				StopID:           strings.TrimSpace(vehicle.StopID),
				StopName:         strings.TrimSpace(vehicle.StopName),
				DepartureSeconds: vehicle.ArrivalSeconds,
				LiveRowID:        strings.TrimSpace(vehicle.LiveRowID),
				Score:            score,
			},
		})
	}
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].score == items[right].score {
			return items[left].item.ID < items[right].item.ID
		}
		return items[left].score > items[right].score
	})
	if len(items) > maxVehicleCandidates {
		items = items[:maxVehicleCandidates]
	}
	out := make([]VehicleCandidate, 0, len(items))
	for _, item := range items {
		out = append(out, item.item)
	}
	return out
}

func candidateIncidents(incidents []model.IncidentSummary, text string) []IncidentCandidate {
	needle := normalizeText(text)
	type scored struct {
		score int
		item  IncidentCandidate
	}
	items := make([]scored, 0, len(incidents))
	for _, incident := range incidents {
		candidateText := strings.Join([]string{
			incident.ID,
			incident.Scope,
			incident.SubjectID,
			incident.SubjectName,
			incident.StopID,
			incident.LastReportName,
		}, " ")
		score := fuzzyScore(needle, normalizeText(candidateText))
		if score <= 0 && len(incidents) > maxIncidentCandidates {
			continue
		}
		items = append(items, scored{
			score: score,
			item: IncidentCandidate{
				ID:          strings.TrimSpace(incident.ID),
				Scope:       strings.TrimSpace(incident.Scope),
				SubjectName: strings.TrimSpace(incident.SubjectName),
				SubjectID:   strings.TrimSpace(incident.SubjectID),
				StopID:      strings.TrimSpace(incident.StopID),
				Score:       score,
			},
		})
	}
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].score == items[right].score {
			return items[left].item.ID < items[right].item.ID
		}
		return items[left].score > items[right].score
	})
	if len(items) > maxIncidentCandidates {
		items = items[:maxIncidentCandidates]
	}
	out := make([]IncidentCandidate, 0, len(items))
	for _, item := range items {
		out = append(out, item.item)
	}
	return out
}

func vehicleScopeCandidateID(vehicle model.LiveVehicle) string {
	if liveRowID := strings.TrimSpace(vehicle.LiveRowID); liveRowID != "" {
		return strings.Join([]string{
			"live",
			strings.ToLower(strings.TrimSpace(vehicle.Mode)),
			strings.TrimSpace(vehicle.RouteLabel),
			strings.TrimSpace(vehicle.Direction),
			liveRowID,
		}, ":")
	}
	return strings.Join([]string{
		"fallback",
		strings.ToLower(strings.TrimSpace(vehicle.Mode)),
		strings.TrimSpace(vehicle.RouteLabel),
		strings.TrimSpace(vehicle.Direction),
		strings.ToLower(strings.TrimSpace(vehicle.Destination)),
	}, ":")
}

func normalizeText(value string) string {
	replacer := strings.NewReplacer(
		"ā", "a", "č", "c", "ē", "e", "ģ", "g", "ī", "i", "ķ", "k", "ļ", "l", "ņ", "n", "š", "s", "ū", "u", "ž", "z",
		"Ā", "a", "Č", "c", "Ē", "e", "Ģ", "g", "Ī", "i", "Ķ", "k", "Ļ", "l", "Ņ", "n", "Š", "s", "Ū", "u", "Ž", "z",
		"ё", "е", "Ё", "е",
	)
	clean := strings.ToLower(replacer.Replace(value))
	var b strings.Builder
	space := false
	for _, r := range clean {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func fuzzyScore(needle, candidate string) int {
	needle = strings.TrimSpace(needle)
	candidate = strings.TrimSpace(candidate)
	if needle == "" || candidate == "" {
		return 0
	}
	score := 0
	if strings.Contains(candidate, needle) || strings.Contains(needle, candidate) {
		score += 8
	}
	candidateWords := strings.Fields(candidate)
	for _, token := range strings.Fields(needle) {
		if len([]rune(token)) < 3 {
			continue
		}
		for _, word := range candidateWords {
			if strings.Contains(word, token) || strings.Contains(token, word) {
				score += 3
				continue
			}
			if len([]rune(word)) >= 4 && levenshteinWithin(token, word, 2) {
				score += 1
			}
		}
	}
	return score
}

func levenshteinWithin(a, b string, maxDistance int) bool {
	ar := []rune(a)
	br := []rune(b)
	if len(ar)-len(br) > maxDistance || len(br)-len(ar) > maxDistance {
		return false
	}
	prev := make([]int, len(br)+1)
	for i := range prev {
		prev[i] = i
	}
	for i, ra := range ar {
		curr := make([]int, len(br)+1)
		curr[0] = i + 1
		rowMin := curr[0]
		for j, rb := range br {
			cost := 0
			if ra != rb {
				cost = 1
			}
			curr[j+1] = minInt(curr[j]+1, prev[j+1]+1, prev[j]+cost)
			if curr[j+1] < rowMin {
				rowMin = curr[j+1]
			}
		}
		if rowMin > maxDistance {
			return false
		}
		prev = curr
	}
	return prev[len(br)] <= maxDistance
}

func minInt(first int, rest ...int) int {
	out := first
	for _, value := range rest {
		if value < out {
			out = value
		}
	}
	return out
}
