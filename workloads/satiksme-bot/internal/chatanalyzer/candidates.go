package chatanalyzer

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"satiksmebot/internal/model"
)

const (
	maxStopCandidates     = 8
	maxVehicleCandidates  = 8
	maxAreaCandidates     = 6
	maxIncidentCandidates = 8
)

func BuildCandidateContext(catalog *model.Catalog, vehicles []model.LiveVehicle, incidents []model.IncidentSummary, text string) CandidateContext {
	stops := candidateStops(catalog, text)
	return CandidateContext{
		Stops:     stops,
		Vehicles:  candidateVehicles(catalog, vehicles, text, stops),
		Areas:     candidateAreas(text, stops),
		Incidents: candidateIncidents(incidents, text),
	}
}

func BuildStopDirectory(catalog *model.Catalog) []StopCandidate {
	if catalog == nil {
		return nil
	}
	items := make([]StopCandidate, 0, len(catalog.Stops))
	seen := make(map[string]struct{}, len(catalog.Stops))
	for _, stop := range catalog.Stops {
		id := strings.TrimSpace(stop.ID)
		name := strings.TrimSpace(stop.Name)
		if id == "" || name == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		items = append(items, StopCandidate{
			ID:   id,
			Name: name,
		})
	}
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].Name == items[right].Name {
			return items[left].ID < items[right].ID
		}
		return items[left].Name < items[right].Name
	})
	return items
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
		candidateText := normalizeText(strings.Join(candidateParts, " "))
		score := fuzzyScore(needle, candidateText)
		stopName := normalizeText(stop.Name)
		if len([]rune(stopName)) >= 4 && strings.Contains(needle, stopName) {
			score += 20
		}
		score += stopNameTokenBoost(needle, stopName)
		if score > 0 && weakDirectionalCentralMatch(needle, candidateText) {
			continue
		}
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
				Latitude:    stop.Latitude,
				Longitude:   stop.Longitude,
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

func stopNameTokenBoost(needle, stopName string) int {
	boost := 0
	for _, token := range strings.Fields(stopName) {
		if genericStopNameToken(token) || len([]rune(token)) < 5 {
			continue
		}
		if strings.Contains(needle, token) {
			boost += 24
		}
	}
	return boost
}

func genericStopNameToken(token string) bool {
	switch token {
	case "iela", "ielas", "bulvaris", "gatve", "laukums", "stacija", "tirgus", "parks":
		return true
	default:
		return false
	}
}

func candidateAreas(text string, stops []StopCandidate) []AreaCandidate {
	scoredStops := make([]StopCandidate, 0, len(stops))
	for _, stop := range stops {
		if stop.Score <= 0 || !hasCoordinates(stop.Latitude, stop.Longitude) {
			continue
		}
		scoredStops = append(scoredStops, stop)
	}
	if len(scoredStops) == 0 {
		return nil
	}

	type scored struct {
		score int
		item  AreaCandidate
	}
	items := make([]scored, 0, len(scoredStops)+2)
	for _, item := range sameNameAreaCandidates(text, scoredStops) {
		items = append(items, scored{score: item.Score, item: item})
	}
	approx := looksLikeApproximateAreaText(text)
	for _, stop := range scoredStops {
		radius := 250
		if approx || stop.Score < 8 {
			radius = 500
		}
		description := areaReportDescription(text, "near "+stop.Name)
		items = append(items, scored{
			score: stop.Score,
			item: AreaCandidate{
				ID:           "near:" + stop.ID,
				Label:        "near " + stop.Name,
				Latitude:     stop.Latitude,
				Longitude:    stop.Longitude,
				RadiusMeters: radius,
				Description:  description,
				Anchors:      []string{stop.Name},
				Score:        stop.Score,
			},
		})
	}
	if looksLikeBetweenAreaText(text) && len(scoredStops) >= 2 {
		for left := 0; left < len(scoredStops) && left < 4; left++ {
			for right := left + 1; right < len(scoredStops) && right < 4; right++ {
				a := scoredStops[left]
				b := scoredStops[right]
				distance := distanceMeters(a.Latitude, a.Longitude, b.Latitude, b.Longitude)
				if distance <= 0 || distance > 1000 {
					continue
				}
				radius := int(math.Round(float64(distance)/2)) + 100
				if radius < 250 {
					radius = 250
				}
				if radius > 500 {
					radius = 500
				}
				description := areaReportDescription(text, "between "+a.Name+" and "+b.Name)
				items = append(items, scored{
					score: a.Score + b.Score + 20,
					item: AreaCandidate{
						ID:           "between:" + a.ID + ":" + b.ID,
						Label:        "between " + a.Name + " and " + b.Name,
						Latitude:     (a.Latitude + b.Latitude) / 2,
						Longitude:    (a.Longitude + b.Longitude) / 2,
						RadiusMeters: radius,
						Description:  description,
						Anchors:      []string{a.Name, b.Name},
						Score:        a.Score + b.Score + 20,
					},
				})
			}
		}
	}
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].score == items[right].score {
			return items[left].item.ID < items[right].item.ID
		}
		return items[left].score > items[right].score
	})
	if len(items) > maxAreaCandidates {
		items = items[:maxAreaCandidates]
	}
	out := make([]AreaCandidate, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, ok := seen[item.item.ID]; ok {
			continue
		}
		seen[item.item.ID] = struct{}{}
		out = append(out, item.item)
	}
	return out
}

func sameNameAreaCandidates(text string, stops []StopCandidate) []AreaCandidate {
	groups := make(map[string][]StopCandidate)
	for _, stop := range stops {
		key := stopNameKey(stop.Name)
		if key == "" || !hasCoordinates(stop.Latitude, stop.Longitude) {
			continue
		}
		groups[key] = append(groups[key], stop)
	}
	out := make([]AreaCandidate, 0)
	for key, group := range groups {
		if len(group) < 2 {
			continue
		}
		lat, lon := averageStopCoordinate(group)
		if !hasCoordinates(lat, lon) {
			continue
		}
		maxDistance := 0
		score := 30
		names := make([]string, 0, len(group))
		seenNames := make(map[string]struct{}, len(group))
		for _, stop := range group {
			distance := distanceMeters(lat, lon, stop.Latitude, stop.Longitude)
			if distance > maxDistance {
				maxDistance = distance
			}
			score += stop.Score
			if _, ok := seenNames[stop.Name]; !ok {
				seenNames[stop.Name] = struct{}{}
				names = append(names, stop.Name)
			}
		}
		if maxDistance > 500 {
			continue
		}
		radius := maxDistance + 100
		if radius < 250 {
			radius = 250
		}
		if radius > 500 {
			radius = 500
		}
		label := group[0].Name + " area"
		out = append(out, AreaCandidate{
			ID:           "name:" + strings.ReplaceAll(key, " ", "-"),
			Label:        label,
			Latitude:     lat,
			Longitude:    lon,
			RadiusMeters: radius,
			Description:  areaReportDescription(text, label),
			Anchors:      names,
			Score:        score,
		})
	}
	return out
}

func averageStopCoordinate(stops []StopCandidate) (float64, float64) {
	var latSum, lonSum float64
	count := 0
	for _, stop := range stops {
		if !hasCoordinates(stop.Latitude, stop.Longitude) {
			continue
		}
		latSum += stop.Latitude
		lonSum += stop.Longitude
		count++
	}
	if count == 0 {
		return 0, 0
	}
	return latSum / float64(count), lonSum / float64(count)
}

func stopNameKey(name string) string {
	return normalizeText(name)
}

func looksLikeApproximateAreaText(text string) bool {
	clean := normalizeText(text)
	if clean == "" {
		return false
	}
	for _, needle := range []string{
		"starp", "between", "mezdu", "mezhdu", "между", "posma", "rajona", "area", "nearby", "around",
		"tunel", "tunnel", "tilt", "bridge", "most", "мост", "под мостом", "zem tilta", "under bridge",
		"pirms", "pec", "after", "before", "posle", "peredish",
	} {
		if strings.Contains(clean, normalizeText(needle)) {
			return true
		}
	}
	return false
}

func looksLikeBetweenAreaText(text string) bool {
	clean := normalizeText(text)
	if clean == "" {
		return false
	}
	for _, needle := range []string{"starp", "between", "mezdu", "mezhdu", "между"} {
		if strings.Contains(clean, normalizeText(needle)) {
			return true
		}
	}
	return false
}

func candidateVehicles(catalog *model.Catalog, vehicles []model.LiveVehicle, text string, stops []StopCandidate) []VehicleCandidate {
	needle := normalizeText(text)
	routeMentions := routeLabelsFromText(needle)
	stopMatches := matchedCatalogStops(catalog, stops)
	type scored struct {
		score int
		item  VehicleCandidate
	}
	items := make([]scored, 0, len(vehicles))
	for _, vehicle := range vehicles {
		scopeID := vehicleScopeCandidateID(vehicle)
		destination := strings.TrimSpace(vehicle.Destination)
		if destination == "" {
			destination = vehicleDestinationFromCatalog(catalog, vehicle)
		}
		candidateText := strings.Join([]string{
			scopeID,
			vehicle.ID,
			vehicle.VehicleCode,
			vehicle.Mode,
			vehicle.RouteLabel,
			vehicle.Direction,
			destination,
			vehicle.StopID,
			vehicle.StopName,
			vehicle.LiveRowID,
		}, " ")
		score := fuzzyScore(needle, normalizeText(candidateText))
		if _, ok := routeMentions[normalizeRouteLabel(vehicle.RouteLabel)]; ok {
			score += 60
		}
		matchedStop, distanceMeters, stopScore := bestVehicleStopMatch(vehicle, stopMatches)
		score += stopScore
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
				Destination:      destination,
				StopID:           strings.TrimSpace(vehicle.StopID),
				StopName:         strings.TrimSpace(vehicle.StopName),
				DepartureSeconds: vehicle.ArrivalSeconds,
				LiveRowID:        strings.TrimSpace(vehicle.LiveRowID),
				Latitude:         vehicle.Latitude,
				Longitude:        vehicle.Longitude,
				MatchedStopID:    strings.TrimSpace(matchedStop.ID),
				MatchedStopName:  strings.TrimSpace(matchedStop.Name),
				DistanceMeters:   distanceMeters,
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

func routeLabelsFromText(normalized string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, token := range strings.Fields(normalized) {
		if label, ok := routeWordLabel(token); ok {
			out[label] = struct{}{}
			continue
		}
		label := normalizeRouteLabel(token)
		if label == "" {
			continue
		}
		digits := 0
		for _, r := range label {
			if unicode.IsDigit(r) {
				digits++
			}
		}
		if digits == 0 || digits > 3 {
			continue
		}
		out[label] = struct{}{}
	}
	return out
}

func routeWordLabel(token string) (string, bool) {
	switch token {
	case "edinice", "edinica", "pervom", "pervoi":
		return "1", true
	case "dvoike", "dvoika", "dvojke", "dvojka":
		return "2", true
	case "troike", "troika", "trojke", "trojka":
		return "3", true
	case "cetverke", "cetverka", "chetverke", "chetverka":
		return "4", true
	case "paterke", "paterka", "pjaterke", "pjaterka":
		return "5", true
	case "sesterke", "sesterka", "shesterke", "shesterka":
		return "6", true
	case "semerke", "semerka":
		return "7", true
	case "vosmerke", "vosmerka":
		return "8", true
	case "devatke", "devatka", "devyatke", "devyatka":
		return "9", true
	default:
		return "", false
	}
}

func normalizeRouteLabel(value string) string {
	value = strings.TrimSpace(strings.ToUpper(value))
	value = strings.NewReplacer("Т", "T", "А", "A").Replace(value)
	var b strings.Builder
	for _, r := range value {
		if unicode.IsDigit(r) || unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func matchedCatalogStops(catalog *model.Catalog, candidates []StopCandidate) []model.Stop {
	if catalog == nil || len(candidates) == 0 {
		return nil
	}
	out := make([]model.Stop, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		stop, ok := model.FindStopByAnyID(catalog, candidate.ID)
		if !ok {
			continue
		}
		id := strings.TrimSpace(stop.ID)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, stop)
	}
	return out
}

func vehicleDestinationFromCatalog(catalog *model.Catalog, vehicle model.LiveVehicle) string {
	if catalog == nil {
		return ""
	}
	routeLabel := normalizeRouteLabel(vehicle.RouteLabel)
	mode := strings.ToLower(strings.TrimSpace(vehicle.Mode))
	if routeLabel == "" {
		return ""
	}
	matches := make([]model.Route, 0, 2)
	for _, route := range catalog.Routes {
		if normalizeRouteLabel(route.Label) != routeLabel {
			continue
		}
		if route.Mode != "" && mode != "" && strings.ToLower(strings.TrimSpace(route.Mode)) != mode {
			continue
		}
		matches = append(matches, route)
	}
	if len(matches) == 0 {
		return ""
	}
	index := 0
	if normalizeVehicleDirection(vehicle.Direction) == "b-a" && len(matches) > 1 {
		index = 1
	}
	return destinationFromRouteName(matches[index].Name, vehicle.Direction)
}

func destinationFromRouteName(name, direction string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return strings.TrimSpace(name)
	}
	if normalizeVehicleDirection(direction) == "b-a" {
		return strings.TrimSpace(parts[len(parts)-1])
	}
	return strings.TrimSpace(parts[len(parts)-1])
}

func normalizeVehicleDirection(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, ">", "-")))
}

func bestVehicleStopMatch(vehicle model.LiveVehicle, stops []model.Stop) (model.Stop, int, int) {
	var best model.Stop
	bestDistance := 0
	bestScore := 0
	for _, stop := range stops {
		score := 0
		distance := 0
		if stop.ID != "" && (model.StopAliasEqual(vehicle.StopID, stop.ID) || model.StopAliasEqual(vehicle.StopID, stop.LiveID)) {
			score = 70
		} else if hasCoordinates(vehicle.Latitude, vehicle.Longitude) && hasCoordinates(stop.Latitude, stop.Longitude) {
			distance = distanceMeters(vehicle.Latitude, vehicle.Longitude, stop.Latitude, stop.Longitude)
			switch {
			case distance <= 150:
				score = 55
			case distance <= 300:
				score = 45
			case distance <= 600:
				score = 30
			case distance <= 1000:
				score = 15
			}
		}
		if score > bestScore {
			best = stop
			bestDistance = distance
			bestScore = score
		}
	}
	return best, bestDistance, bestScore
}

func hasCoordinates(latitude, longitude float64) bool {
	return latitude != 0 && longitude != 0
}

func distanceMeters(lat1, lon1, lat2, lon2 float64) int {
	const earthRadiusMeters = 6371000.0
	toRadians := func(value float64) float64 {
		return value * math.Pi / 180
	}
	phi1 := toRadians(lat1)
	phi2 := toRadians(lat2)
	deltaPhi := toRadians(lat2 - lat1)
	deltaLambda := toRadians(lon2 - lon1)
	a := math.Sin(deltaPhi/2)*math.Sin(deltaPhi/2) +
		math.Cos(phi1)*math.Cos(phi2)*math.Sin(deltaLambda/2)*math.Sin(deltaLambda/2)
	return int(math.Round(earthRadiusMeters * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))))
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
		normalizedCandidate := normalizeText(candidateText)
		score := fuzzyScore(needle, normalizedCandidate)
		if score <= 0 || weakDirectionalCentralMatch(needle, normalizedCandidate) {
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

func weakDirectionalCentralMatch(messageText, candidateText string) bool {
	if !mentionsCentralDirection(messageText) {
		return false
	}
	if mentionsCentralPlace(messageText) {
		return false
	}
	for _, token := range []string{"centraltirgus", "centrala stacija", "central station"} {
		if strings.Contains(candidateText, normalizeText(token)) {
			return true
		}
	}
	return false
}

func mentionsCentralDirection(text string) bool {
	for _, phrase := range []string{
		"iz centra", "no centra", "uz centru", "lidz centram", "centra virziena", "centra virziens",
		"from center", "from centre", "towards center", "towards centre", "to center", "to centre",
		"ot centra", "v centr", "do centra",
	} {
		if strings.Contains(text, normalizeText(phrase)) {
			return true
		}
	}
	return false
}

func mentionsCentralPlace(text string) bool {
	for _, phrase := range []string{
		"centraltirgus", "centralais tirgus", "central market", "tirgus", "rinok",
		"centrala stacija", "central station", "vokzal", "stacija",
	} {
		if strings.Contains(text, normalizeText(phrase)) {
			return true
		}
	}
	return false
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
		"а", "a", "б", "b", "в", "v", "г", "g", "д", "d", "е", "e", "ё", "e", "ж", "z", "з", "z", "и", "i", "й", "i",
		"к", "k", "л", "l", "м", "m", "н", "n", "о", "o", "п", "p", "р", "r", "с", "s", "т", "t", "у", "u", "ф", "f",
		"х", "h", "ц", "c", "ч", "c", "ш", "s", "щ", "s", "ы", "i", "э", "e", "ю", "u", "я", "a", "ь", "", "ъ", "",
		"А", "a", "Б", "b", "В", "v", "Г", "g", "Д", "d", "Е", "e", "Ё", "e", "Ж", "z", "З", "z", "И", "i", "Й", "i",
		"К", "k", "Л", "l", "М", "m", "Н", "n", "О", "o", "П", "p", "Р", "r", "С", "s", "Т", "t", "У", "u", "Ф", "f",
		"Х", "h", "Ц", "c", "Ч", "c", "Ш", "s", "Щ", "s", "Ы", "i", "Э", "e", "Ю", "u", "Я", "a", "Ь", "", "Ъ", "",
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
		if len([]rune(token)) < 3 || genericFuzzyToken(token) {
			continue
		}
		for _, word := range candidateWords {
			if len([]rune(word)) < 3 {
				continue
			}
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

func genericFuzzyToken(token string) bool {
	if genericStopNameToken(token) {
		return true
	}
	switch token {
	case "tira", "cisto", "chisto", "clean", "dirty", "griazno", "grjazno", "netiru", "netirs",
		"virziena", "virziens", "storonu", "centra", "centru", "center", "centre",
		"kontrole", "kontrolleri", "controlleri", "controllers", "reid", "raid":
		return true
	default:
		return false
	}
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
