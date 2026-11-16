package gamesummary

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// AgentIntervention is one agent intervention of a game as the agent.game.summary
// span lists it (#2640). The decision loop's InterventionLog notes it; gamesummary
// cannot import decision (decision imports gamesummary), so main.go maps the two.
type AgentIntervention struct {
	At         time.Time
	Action     string
	Target     string
	TargetName string
	Value      string
	Outcome    string // applied, blocked, gated_off, write_failed, held
	Backend    string
}

// AgentInterventions is everything noted for one game: the entries in decision
// order (capped), how many were dropped over the cap, the held and noop answers,
// and the decide backends behind them (sorted).
type AgentInterventions struct {
	Entries  []AgentIntervention
	Dropped  int
	Held     int
	Noop     int
	Backends []string
}

// Span attributes of the intervention list (#2640).
const (
	attrInterventions          = "summary.interventions"
	attrInterventionsTruncated = "summary.interventions_truncated"
	attrInterventionsHeld      = "summary.interventions_held"
	attrInterventionsNoop      = "summary.interventions_noop"
	attrDecideBackend          = "summary.decide.backend"
)

// interventionJSON is one summary.interventions entry.
type interventionJSON struct {
	OffsetS float64 `json:"offset_s"`
	Action  string  `json:"action"`
	Target  string  `json:"target,omitempty"`
	Name    string  `json:"name,omitempty"`
	Value   string  `json:"value,omitempty"`
	Outcome string  `json:"outcome"`
	Backend string  `json:"backend,omitempty"`
}

// interventionAttributes renders a game's noted interventions as span attributes,
// offsets measured from anchor (the summary's +0s, the first timeline event). None
// when nothing was noted.
func interventionAttributes(iv AgentInterventions, anchor time.Time) []attribute.KeyValue {
	if len(iv.Entries) == 0 && iv.Noop == 0 {
		return nil
	}
	attrs := []attribute.KeyValue{
		attribute.Int(attrInterventionsHeld, iv.Held),
		attribute.Int(attrInterventionsNoop, iv.Noop),
	}
	if len(iv.Backends) > 0 {
		attrs = append(attrs, attribute.String(attrDecideBackend, strings.Join(iv.Backends, ",")))
	}
	if len(iv.Entries) > 0 {
		list := make([]interventionJSON, 0, len(iv.Entries))
		for _, e := range iv.Entries {
			list = append(list, interventionJSON{
				OffsetS: math.Round(offset(e.At, anchor)*10) / 10,
				Action:  e.Action,
				Target:  e.Target,
				Name:    e.TargetName,
				Value:   e.Value,
				Outcome: e.Outcome,
				Backend: e.Backend,
			})
		}
		if raw, err := json.Marshal(list); err == nil {
			attrs = append(attrs, attribute.String(attrInterventions, string(raw)))
		}
	}
	if iv.Dropped > 0 {
		attrs = append(attrs, attribute.Bool(attrInterventionsTruncated, true))
	}
	return attrs
}
