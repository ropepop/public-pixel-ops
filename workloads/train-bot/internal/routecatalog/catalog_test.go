package routecatalog

import (
	"testing"
	"time"

	"telegramtrainapp/internal/domain"
)

func TestBuildCoversSeededRoutesAndAllStations(t *testing.T) {
	base := time.Date(2026, 4, 27, 7, 0, 0, 0, time.UTC)
	stations := []domain.Station{
		{ID: "riga", Name: "Rīga"},
		{ID: "tukums", Name: "Tukums"},
		{ID: "skulte", Name: "Skulte"},
		{ID: "valga", Name: "Valga"},
		{ID: "jelgava", Name: "Jelgava"},
		{ID: "liepaja", Name: "Liepāja"},
		{ID: "aizkraukle", Name: "Aizkraukle"},
		{ID: "daugavpils", Name: "Daugavpils"},
		{ID: "small-stop", Name: "Small Stop"},
	}
	trains := []domain.TrainInstance{
		trainForRoute("tukums-line", "Rīga", "Tukums", base),
		trainForRoute("skulte-line", "Rīga", "Skulte", base.Add(10*time.Minute)),
		trainForRoute("valga-line", "Rīga", "Valga", base.Add(20*time.Minute)),
		trainForRoute("liepaja-line", "Rīga", "Liepāja", base.Add(30*time.Minute)),
		trainForRoute("east-line", "Rīga", "Daugavpils", base.Add(40*time.Minute)),
		trainForRoute("small-line", "Rīga", "Small Stop", base.Add(50*time.Minute)),
	}
	stops := []domain.TrainStop{
		stop("tukums-line", "riga", "Rīga", 1),
		stop("tukums-line", "tukums", "Tukums", 2),
		stop("skulte-line", "riga", "Rīga", 1),
		stop("skulte-line", "skulte", "Skulte", 2),
		stop("valga-line", "riga", "Rīga", 1),
		stop("valga-line", "valga", "Valga", 2),
		stop("liepaja-line", "riga", "Rīga", 1),
		stop("liepaja-line", "jelgava", "Jelgava", 2),
		stop("liepaja-line", "liepaja", "Liepāja", 3),
		stop("east-line", "riga", "Rīga", 1),
		stop("east-line", "aizkraukle", "Aizkraukle", 2),
		stop("east-line", "daugavpils", "Daugavpils", 3),
		stop("small-line", "riga", "Rīga", 1),
		stop("small-line", "small-stop", "Small Stop", 2),
	}

	routes := Build(stations, trains, stops)
	byID := map[string]domain.RouteCheckInRoute{}
	covered := map[string]struct{}{}
	for _, route := range routes {
		byID[route.ID] = route
		for _, stationID := range route.StationIDs {
			covered[stationID] = struct{}{}
		}
	}

	for _, id := range []string{
		"riga-tukums-jurmala",
		"riga-skulte",
		"riga-sigulda-valga",
		"riga-jelgava-liepaja",
		"riga-aizkraukle-east",
	} {
		if route, ok := byID[id]; !ok || len(route.StationIDs) == 0 {
			t.Fatalf("expected seeded route %s, got %+v", id, routes)
		}
	}
	for _, station := range stations {
		if _, ok := covered[station.ID]; !ok {
			t.Fatalf("expected station %s to be covered by route catalog %+v", station.ID, routes)
		}
	}
	if len(routes) <= len(seedRoutes) {
		t.Fatalf("expected supplemental route for uncovered station, got %+v", routes)
	}
}

func trainForRoute(id, from, to string, dep time.Time) domain.TrainInstance {
	return domain.TrainInstance{
		ID:          id,
		ServiceDate: dep.Format("2006-01-02"),
		FromStation: from,
		ToStation:   to,
		DepartureAt: dep,
		ArrivalAt:   dep.Add(45 * time.Minute),
	}
}

func stop(trainID, stationID, stationName string, seq int) domain.TrainStop {
	return domain.TrainStop{
		TrainInstanceID: trainID,
		StationID:       stationID,
		StationName:     stationName,
		Seq:             seq,
	}
}
