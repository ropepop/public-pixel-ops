package routecatalog

import (
	"sort"
	"strings"

	"telegramtrainapp/internal/domain"
	"telegramtrainapp/internal/stationsearch"
)

type seedRoute struct {
	ID       string
	Name     string
	Keywords []string
}

var seedRoutes = []seedRoute{
	{
		ID:       "riga-tukums-jurmala",
		Name:     "Rīga - Jūrmala - Tukums",
		Keywords: []string{"tukums", "jurmala", "dubulti", "sloka", "kemeri"},
	},
	{
		ID:       "riga-skulte",
		Name:     "Rīga - Skulte",
		Keywords: []string{"skulte", "saulkrasti", "carnikava"},
	},
	{
		ID:       "riga-sigulda-valga",
		Name:     "Rīga - Sigulda - Valga",
		Keywords: []string{"sigulda", "cesis", "valmiera", "valga", "valka", "ligatne"},
	},
	{
		ID:       "riga-jelgava-liepaja",
		Name:     "Rīga - Jelgava - Liepāja",
		Keywords: []string{"jelgava", "dobele", "saldus", "liepaja", "biksti", "skundra"},
	},
	{
		ID:   "riga-aizkraukle-east",
		Name: "Rīga - Aizkraukle - Austrumi",
		Keywords: []string{
			"aizkraukle", "ogre", "lielvarde", "kegums", "krustpils", "jekabpils",
			"daugavpils", "kraslava", "indra", "rezekne", "zilupe", "madona", "gulbene",
		},
	},
}

type routeBuildState struct {
	route        domain.RouteCheckInRoute
	stationSet   map[string]struct{}
	stationOrder map[string]int
}

func Build(stations []domain.Station, trains []domain.TrainInstance, stops []domain.TrainStop) []domain.RouteCheckInRoute {
	stationByID := map[string]domain.Station{}
	stationIDByName := map[string]string{}
	for _, station := range stations {
		id := strings.TrimSpace(station.ID)
		name := strings.TrimSpace(station.Name)
		if id == "" {
			continue
		}
		stationByID[id] = station
		if name != "" {
			stationIDByName[stationKey(name)] = id
		}
		if station.NormalizedKey != "" {
			stationIDByName[stationKey(station.NormalizedKey)] = id
		}
	}

	stopsByTrain := stopsGroupedByTrain(stops, stationByID)
	trains = append([]domain.TrainInstance(nil), trains...)
	sort.SliceStable(trains, func(i, j int) bool {
		if trains[i].DepartureAt.Equal(trains[j].DepartureAt) {
			return trains[i].ID < trains[j].ID
		}
		return trains[i].DepartureAt.Before(trains[j].DepartureAt)
	})

	states := make([]*routeBuildState, 0, len(seedRoutes))
	for _, seed := range seedRoutes {
		states = append(states, &routeBuildState{
			route: domain.RouteCheckInRoute{
				ID:   seed.ID,
				Name: seed.Name,
			},
			stationSet:   map[string]struct{}{},
			stationOrder: map[string]int{},
		})
	}

	for _, train := range trains {
		trainStops := normalizedTrainStops(train, stopsByTrain[train.ID], stationIDByName, stationByID)
		if len(trainStops) == 0 {
			continue
		}
		searchText := trainSearchText(train, trainStops)
		for index, seed := range seedRoutes {
			if !matchesSeed(searchText, seed.Keywords) {
				continue
			}
			addStops(states[index], trainStops)
		}
	}

	covered := map[string]struct{}{}
	routes := make([]domain.RouteCheckInRoute, 0, len(states)+4)
	for _, state := range states {
		route := finalizeRoute(state, stationByID)
		if len(route.StationIDs) == 0 {
			continue
		}
		routes = append(routes, route)
		for _, stationID := range route.StationIDs {
			covered[stationID] = struct{}{}
		}
	}

	for {
		uncovered := uncoveredStationIDs(stations, covered)
		if len(uncovered) == 0 {
			break
		}
		bestTrain, bestStops := bestSupplementalTrain(trains, stopsByTrain, stationIDByName, stationByID, uncovered)
		if len(bestStops) == 0 {
			stationID := uncovered[0]
			state := &routeBuildState{
				route: domain.RouteCheckInRoute{
					ID:           "station-" + slug(stationID),
					Name:         stationDisplayName(stationByID, stationID),
					Supplemental: true,
				},
				stationSet:   map[string]struct{}{},
				stationOrder: map[string]int{},
			}
			addStops(state, []domain.TrainStop{{StationID: stationID, StationName: stationDisplayName(stationByID, stationID), Seq: 1}})
			route := finalizeRoute(state, stationByID)
			routes = append(routes, route)
			covered[stationID] = struct{}{}
			continue
		}
		state := &routeBuildState{
			route: domain.RouteCheckInRoute{
				ID:           "route-" + slug(bestTrain.FromStation+"-"+bestTrain.ToStation),
				Name:         strings.TrimSpace(bestTrain.FromStation + " - " + bestTrain.ToStation),
				Supplemental: true,
			},
			stationSet:   map[string]struct{}{},
			stationOrder: map[string]int{},
		}
		addStops(state, bestStops)
		route := finalizeRoute(state, stationByID)
		routes = append(routes, route)
		for _, stationID := range route.StationIDs {
			covered[stationID] = struct{}{}
		}
	}

	dedupeRouteIDs(routes)
	return routes
}

func stopsGroupedByTrain(stops []domain.TrainStop, stationByID map[string]domain.Station) map[string][]domain.TrainStop {
	grouped := map[string][]domain.TrainStop{}
	for _, stop := range stops {
		trainID := strings.TrimSpace(stop.TrainInstanceID)
		stationID := strings.TrimSpace(stop.StationID)
		if trainID == "" || stationID == "" {
			continue
		}
		if strings.TrimSpace(stop.StationName) == "" {
			stop.StationName = stationDisplayName(stationByID, stationID)
		}
		grouped[trainID] = append(grouped[trainID], stop)
	}
	for trainID := range grouped {
		sortStops(grouped[trainID])
	}
	return grouped
}

func normalizedTrainStops(train domain.TrainInstance, stops []domain.TrainStop, stationIDByName map[string]string, stationByID map[string]domain.Station) []domain.TrainStop {
	if len(stops) > 0 {
		out := append([]domain.TrainStop(nil), stops...)
		sortStops(out)
		return out
	}
	out := make([]domain.TrainStop, 0, 2)
	if id := stationIDByName[stationKey(train.FromStation)]; id != "" {
		out = append(out, domain.TrainStop{TrainInstanceID: train.ID, StationID: id, StationName: stationDisplayName(stationByID, id), Seq: 1})
	}
	if id := stationIDByName[stationKey(train.ToStation)]; id != "" {
		out = append(out, domain.TrainStop{TrainInstanceID: train.ID, StationID: id, StationName: stationDisplayName(stationByID, id), Seq: 2})
	}
	return out
}

func trainSearchText(train domain.TrainInstance, stops []domain.TrainStop) string {
	parts := []string{stationKey(train.FromStation), stationKey(train.ToStation)}
	for _, stop := range stops {
		parts = append(parts, stationKey(stop.StationName))
	}
	return strings.Join(parts, " ")
}

func matchesSeed(searchText string, keywords []string) bool {
	for _, keyword := range keywords {
		if strings.Contains(searchText, stationKey(keyword)) {
			return true
		}
	}
	return false
}

func addStops(state *routeBuildState, stops []domain.TrainStop) {
	for index, stop := range stops {
		stationID := strings.TrimSpace(stop.StationID)
		if stationID == "" {
			continue
		}
		state.stationSet[stationID] = struct{}{}
		order := stop.Seq
		if order <= 0 {
			order = index + 1
		}
		existing, ok := state.stationOrder[stationID]
		if !ok || order < existing {
			state.stationOrder[stationID] = order
		}
	}
}

func finalizeRoute(state *routeBuildState, stationByID map[string]domain.Station) domain.RouteCheckInRoute {
	stationIDs := make([]string, 0, len(state.stationSet))
	for stationID := range state.stationSet {
		stationIDs = append(stationIDs, stationID)
	}
	sort.SliceStable(stationIDs, func(i, j int) bool {
		leftOrder := state.stationOrder[stationIDs[i]]
		rightOrder := state.stationOrder[stationIDs[j]]
		if leftOrder == rightOrder {
			return stationDisplayName(stationByID, stationIDs[i]) < stationDisplayName(stationByID, stationIDs[j])
		}
		return leftOrder < rightOrder
	})
	names := make([]string, 0, len(stationIDs))
	for _, stationID := range stationIDs {
		names = append(names, stationDisplayName(stationByID, stationID))
	}
	route := state.route
	route.StationIDs = stationIDs
	route.StationNames = names
	route.StationCount = len(stationIDs)
	return route
}

func uncoveredStationIDs(stations []domain.Station, covered map[string]struct{}) []string {
	out := make([]string, 0)
	for _, station := range stations {
		stationID := strings.TrimSpace(station.ID)
		if stationID == "" {
			continue
		}
		if _, ok := covered[stationID]; ok {
			continue
		}
		out = append(out, stationID)
	}
	sort.Strings(out)
	return out
}

func bestSupplementalTrain(trains []domain.TrainInstance, stopsByTrain map[string][]domain.TrainStop, stationIDByName map[string]string, stationByID map[string]domain.Station, uncovered []string) (domain.TrainInstance, []domain.TrainStop) {
	uncoveredSet := map[string]struct{}{}
	for _, stationID := range uncovered {
		uncoveredSet[stationID] = struct{}{}
	}
	var bestTrain domain.TrainInstance
	var bestStops []domain.TrainStop
	bestCount := 0
	for _, train := range trains {
		stops := normalizedTrainStops(train, stopsByTrain[train.ID], stationIDByName, stationByID)
		count := 0
		for _, stop := range stops {
			if _, ok := uncoveredSet[strings.TrimSpace(stop.StationID)]; ok {
				count++
			}
		}
		if count > bestCount {
			bestCount = count
			bestTrain = train
			bestStops = stops
		}
	}
	return bestTrain, bestStops
}

func dedupeRouteIDs(routes []domain.RouteCheckInRoute) {
	seen := map[string]int{}
	for index := range routes {
		base := routes[index].ID
		if base == "" {
			base = "route"
		}
		count := seen[base]
		seen[base] = count + 1
		if count > 0 {
			routes[index].ID = base + "-" + slug(routes[index].Name)
		}
	}
}

func sortStops(stops []domain.TrainStop) {
	sort.SliceStable(stops, func(i, j int) bool {
		if stops[i].Seq == stops[j].Seq {
			return stops[i].StationID < stops[j].StationID
		}
		return stops[i].Seq < stops[j].Seq
	})
}

func stationDisplayName(stationByID map[string]domain.Station, stationID string) string {
	if station, ok := stationByID[stationID]; ok && strings.TrimSpace(station.Name) != "" {
		return strings.TrimSpace(station.Name)
	}
	return strings.TrimSpace(stationID)
}

func stationKey(value string) string {
	return stationsearch.Normalize(value)
}

func slug(value string) string {
	value = stationKey(value)
	if value == "" {
		return "route"
	}
	parts := make([]string, 0, len(value))
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			parts = append(parts, string(r))
			continue
		}
		parts = append(parts, "-")
	}
	return strings.Trim(strings.Join(parts, ""), "-")
}
