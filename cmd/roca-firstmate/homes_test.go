package main

import (
	"errors"
	"testing"
)

func TestPairHomesMatchesFlagsPositionally(t *testing.T) {
	pairs, err := pairHomes(
		[]string{"/harbor", "/skiff"},
		[]string{"northwind-harbor", "skiff-secondmate"},
		[]string{"Harbor", "Skiff"},
		[]string{"primary", "secondmate"},
	)
	if err != nil {
		t.Fatalf("pairHomes: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("pairs = %d, want 2", len(pairs))
	}
	if pairs[0].Path != "/harbor" || pairs[0].ID != "northwind-harbor" || pairs[0].Kind != "primary" {
		t.Fatalf("first pair %+v", pairs[0])
	}
	if pairs[1].Path != "/skiff" || pairs[1].ID != "skiff-secondmate" || pairs[1].Kind != "secondmate" {
		t.Fatalf("second pair %+v", pairs[1])
	}
	if pairs[0].Label != "Harbor" || pairs[1].Label != "Skiff" {
		t.Fatalf("labels %+v", pairs)
	}
}

func TestPairHomesRefusesUnbalancedFlags(t *testing.T) {
	_, err := pairHomes([]string{"/harbor"}, []string{"a", "b"}, nil, nil)
	if !errors.Is(err, errUnbalancedHomes) {
		t.Fatalf("two home-ids: err = %v, want %v", err, errUnbalancedHomes)
	}
	_, err = pairHomes([]string{"/harbor", "/skiff"}, []string{"a"}, nil, nil)
	if !errors.Is(err, errUnbalancedHomes) {
		t.Fatalf("two homes: err = %v, want %v", err, errUnbalancedHomes)
	}
}

func TestPairHomesDefaultsKindAndBroadcastsASingleKind(t *testing.T) {
	pairs, err := pairHomes([]string{"/a", "/b"}, []string{"a", "b"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pairs[0].Kind != "primary" || pairs[1].Kind != "primary" {
		t.Fatalf("default kind %+v", pairs)
	}
	pairs, err = pairHomes([]string{"/a", "/b"}, []string{"a", "b"}, nil, []string{"secondmate"})
	if err != nil {
		t.Fatal(err)
	}
	if pairs[0].Kind != "secondmate" || pairs[1].Kind != "secondmate" {
		t.Fatalf("broadcast kind %+v", pairs)
	}
}

func TestResolveChartFollowHomesTreatsBareHomeIDsAsFilter(t *testing.T) {
	req, err := resolveHomes(homeBinding{homeIDs: stringList{"skiff-secondmate"}}, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Pairs) != 0 || len(req.Filters) != 1 || req.Filters[0] != "skiff-secondmate" {
		t.Fatalf("filter-only request %+v", req)
	}
	req, err = resolveHomes(homeBinding{}, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Pairs) != 0 || len(req.Filters) != 0 {
		t.Fatalf("all-homes request %+v", req)
	}
}

func TestResolveBareHomeIDIgnoresEnvironmentHome(t *testing.T) {
	req, err := resolveHomes(
		homeBinding{homeIDs: stringList{"skiff-secondmate"}},
		"/harbor",
		"northwind-harbor",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Pairs) != 0 || len(req.Filters) != 1 || req.Filters[0] != "skiff-secondmate" {
		t.Fatalf("filter-only request %+v", req)
	}
}

func TestResolveExplicitHomeDoesNotUseEnvironmentID(t *testing.T) {
	_, err := resolveHomes(
		homeBinding{homes: stringList{"/skiff"}},
		"/harbor",
		"northwind-harbor",
		false,
	)
	if !errors.Is(err, errUnbalancedHomes) {
		t.Fatalf("explicit home: err = %v, want %v", err, errUnbalancedHomes)
	}
}

func TestResolveHomesRejectsBlankHomeID(t *testing.T) {
	_, err := resolveHomes(homeBinding{homeIDs: stringList{" "}}, "", "", true)
	if !errors.Is(err, errEmptyHomeID) {
		t.Fatalf("blank home-id: err = %v, want %v", err, errEmptyHomeID)
	}
}

func TestResolveHomesSeedsASinglePairFromEnv(t *testing.T) {
	req, err := resolveHomes(homeBinding{}, "/harbor", "northwind-harbor", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Pairs) != 1 || req.Pairs[0].Path != "/harbor" || req.Pairs[0].ID != "northwind-harbor" {
		t.Fatalf("env pair %+v", req.Pairs)
	}
}
