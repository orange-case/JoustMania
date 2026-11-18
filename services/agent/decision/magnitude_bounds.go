package decision

import (
	"context"
	"strconv"

	"go.opentelemetry.io/otel/attribute"

	"github.com/joustmania/agent/decide"
)

// The game's magnitude ranges (#2696, FR-098, UC-040 BR-045, decide-contract.md
// §2.1, §12): decision.magnitude_bounds, operator-written (C-034), read once per
// decide request and carried on Request.Bounds so every consumer uses one table.
const (
	// AttrDecideMagnitudeBoundsSource is "flag" when the request's ranges differ
	// from the code default table, else "default"; on every agent.decide.
	AttrDecideMagnitudeBoundsSource = "decide.magnitude_bounds.source"
	// AttrDecideMagnitudeBoundsInvalid names the interventions whose flag entry
	// fell back to the code default (sorted, comma-separated); only when one did.
	AttrDecideMagnitudeBoundsInvalid = "decide.magnitude_bounds.invalid"
)

// MagnitudeBoundsSource returns one game's magnitude ranges. main.go wires
// flags.Flags.MagnitudeBounds, which gives the code default table on any error.
type MagnitudeBoundsSource func(ctx context.Context, gameID string) *decide.MagnitudeTable

// SetMagnitudeBoundsSource injects the magnitude-ranges source of the decide
// requests. nil (the default and every older test) leaves Request.Bounds nil: the
// code default ranges. Set at construction, before the loop serves.
func (l *Loop) SetMagnitudeBoundsSource(src MagnitudeBoundsSource) { l.magnitudeBounds = src }

// magnitudeBoundsFor is the request's table: the source's, nil without a source.
func (l *Loop) magnitudeBoundsFor(ctx context.Context, gameID string) *decide.MagnitudeTable {
	if l.magnitudeBounds == nil {
		return nil
	}
	return l.magnitudeBounds(ctx, gameID)
}

// magnitudeBoundsAttributes are the agent.decide attributes of a request's ranges.
func magnitudeBoundsAttributes(t *decide.MagnitudeTable) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String(AttrDecideMagnitudeBoundsSource, t.Source())}
	if s := t.InvalidString(); s != "" {
		attrs = append(attrs, attribute.String(AttrDecideMagnitudeBoundsInvalid, s))
	}
	return attrs
}

// clampRulesValue clamps a rules decision's magnitude into the request's range
// (UC-040 BR-045): a value outside it is set to the nearest end, and an empty value
// (the writer default) is made explicit only when the code default is outside the
// range. A value inside the range, an unparsable value and an intervention without
// bounds are unchanged, so at the default ranges no rules decision moves.
func clampRulesValue(t *decide.MagnitudeTable, d Decision) Decision {
	b, ok := t.Lookup(d.Intervention)
	if !ok {
		return d
	}
	v := b.Default
	if def, hasDef := decide.MagnitudeBounds(d.Intervention); hasDef && d.Value == "" {
		v = def.Default
	} else if d.Value != "" {
		parsed, err := strconv.ParseFloat(d.Value, 64)
		if err != nil {
			return d
		}
		v = parsed
	}
	c, _ := t.Clamp(d.Intervention, v)
	if c == v {
		return d
	}
	d.Value = strconv.FormatFloat(c, 'g', -1, 64)
	return d
}
