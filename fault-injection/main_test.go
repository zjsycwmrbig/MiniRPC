package main

import (
	"slices"
	"testing"
)

func TestSelectScenariosRunsEveryRegisteredDrillForAll(t *testing.T) {
	registry := scenarioRegistry()
	names, err := selectScenarios("all", registry)
	if err != nil {
		t.Fatalf("selectScenarios(all) error = %v", err)
	}
	for _, name := range names {
		if _, ok := registry[name]; !ok {
			t.Fatalf("selected scenario %q is not registered", name)
		}
	}
	if len(names) != len(registry) {
		t.Fatalf("selected %d scenarios, want all %d registered", len(names), len(registry))
	}
	if !slices.Contains(names, "instance-failover") || !slices.Contains(names, "registry-outage") {
		t.Fatalf("default scenarios = %v, missing required recovery drills", names)
	}
}

func TestSelectScenariosRejectsUnknownName(t *testing.T) {
	_, err := selectScenarios("instance-failover,does-not-exist", scenarioRegistry())
	if err == nil {
		t.Fatal("selectScenarios() accepted unknown scenario")
	}
}
