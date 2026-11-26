package main

import (
	"context"
	"testing"

	"github.com/joustmania/agent/flags"
)

// UC: UC-045 A1
// UC: UC-043 Pre-1
// Autonomous promotion writes a real default, so only `enabled` = on may open
// it: watch, off, a bool-era value and an unreachable flagd all keep it shut.
func TestPromotionConfigResolverEnabledOnlyAtOn(t *testing.T) {
	cases := []struct {
		name string
		ev   *switchableEvaluator
		want bool
	}{
		{"on", &switchableEvaluator{strings: map[string]string{"enabled": "on"}}, true},
		{"watch", &switchableEvaluator{strings: map[string]string{"enabled": "watch"}}, false},
		{"off", &switchableEvaluator{strings: map[string]string{"enabled": "off"}}, false},
		{"bool-era text true", &switchableEvaluator{strings: map[string]string{"enabled": "true"}}, false},
		{"bool-era bool true", &switchableEvaluator{booleans: map[string]bool{"enabled": true}}, false},
		{"flagd unreachable", &switchableEvaluator{strings: map[string]string{"enabled": "on"}, unreachable: true}, false},
	}
	for _, c := range cases {
		t.Run("UC-045/A1 "+c.name, func(t *testing.T) {
			cfg := promotionConfigResolver(flags.New(c.ev, nil))(context.Background())
			if cfg.Enabled != c.want {
				t.Fatalf("promote.Config.Enabled = %v at %s, want %v", cfg.Enabled, c.name, c.want)
			}
		})
	}
}
