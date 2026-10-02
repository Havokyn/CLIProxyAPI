package management

import "testing"

func TestNormalizeRoutingStrategyWeightedRoundRobin(t *testing.T) {
	for _, input := range []string{"weighted-round-robin", "weightedroundrobin", "wrr"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "weighted-round-robin" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want weighted-round-robin, true", input, got, ok)
		}
	}
}

func TestNormalizeRoutingStrategyResetAware(t *testing.T) {
	for _, input := range []string{"reset-aware", "resetaware", "quota-reset-aware"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "reset-aware" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want reset-aware, true", input, got, ok)
		}
	}
}
