package api

import (
	"context"

	"go.opentelemetry.io/otel/attribute"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
	"github.com/joustmania/flag-service/telemetry"
)

// The in-process write entry of the Flagsmith link (services.md §7.3): a
// pulled change goes through the same validation, apply code, span, trace
// link and metrics as PUT / POST / DELETE /{flagSetId}/{flagName}, not over
// HTTP. The writer label is always flagsmith.WriterLabel. Experiment-arm
// and real scopes are refused first, before any other check, with
// 422 flagsmith_scope_read_only (UC-054 BR-003). ctx is the parent of the
// flagservice.write span (the flagservice.flagsmith.pull span that found
// the change).
//
// *API implements flagsmith.LocalWriter with these three methods.

var _ flagsmith.LocalWriter = (*API)(nil)

// scopeReadOnly is the 422 of a pulled write in a scope Flagsmith may only show.
func scopeReadOnly(scopeName string) *fserr.Error {
	switch scopeName {
	case registry.ScopeExperiment, registry.ScopeReal:
		return fserr.Unprocessable(fserr.FlagsmithScopeReadOnly, "scope %s is shown in Flagsmith only; a Flagsmith value never writes it", scopeName)
	}
	return nil
}

// FlagsmithPut is the in-process PUT of a pulled change.
func (a *API) FlagsmithPut(ctx context.Context, req state.WriteRequest) (state.WriteResult, *fserr.Error) {
	start := a.now()
	ctx, span := a.Tracer.Start(ctx, telemetry.SpanWrite)
	a.beginWrite()
	defer a.endWrite()
	req.Writer = flagsmith.WriterLabel
	req.DryRun = false
	res := state.WriteResult{FlagSet: req.Set, Flag: req.Flag, Epoch: a.State.Epoch(), ScopeKey: req.Scope.Name()}
	ferr := scopeReadOnly(res.ScopeKey)
	if ferr == nil {
		res, ferr = a.State.Put(req)
	}
	a.finishWrite(ctx, span, telemetry.OpSet, req, res, ferr, start)
	return res, ferr
}

// FlagsmithAppend is the in-process POST of a pulled event.
func (a *API) FlagsmithAppend(ctx context.Context, req state.AppendRequest) (state.WriteResult, *fserr.Error) {
	start := a.now()
	ctx, span := a.Tracer.Start(ctx, telemetry.SpanWrite)
	a.beginWrite()
	defer a.endWrite()
	req.Writer = flagsmith.WriterLabel
	wreq := state.WriteRequest{Set: req.Set, Flag: req.Flag, Scope: req.Scope, Writer: req.Writer, DecisionID: req.DecisionID}
	res := state.WriteResult{FlagSet: req.Set, Flag: req.Flag, Epoch: a.State.Epoch(), ScopeKey: req.Scope.Name()}
	ferr := scopeReadOnly(res.ScopeKey)
	if ferr == nil {
		res, ferr = a.State.Append(req)
	}
	a.finishWrite(ctx, span, telemetry.OpAppend, wreq, res, ferr, start)
	return res, ferr
}

// FlagsmithClear is the in-process DELETE of a pulled change (StackAll: the
// overlay and the admin value, Flagsmith enabled=false).
func (a *API) FlagsmithClear(ctx context.Context, req state.ClearRequest) (state.WriteResult, *fserr.Error) {
	start := a.now()
	ctx, span := a.Tracer.Start(ctx, telemetry.SpanWrite)
	a.beginWrite()
	defer a.endWrite()
	req.Writer = flagsmith.WriterLabel
	wreq := state.WriteRequest{Set: req.Set, Flag: req.Flag, Scope: req.Scope, Writer: req.Writer}
	res := state.WriteResult{FlagSet: req.Set, Flag: req.Flag, Epoch: a.State.Epoch(), ScopeKey: req.Scope.Name()}
	ferr := scopeReadOnly(res.ScopeKey)
	if ferr == nil {
		res, ferr = a.State.Clear(req)
	}
	span.SetAttributes(attribute.Int("flagservice.cleared", res.Cleared))
	a.finishWrite(ctx, span, telemetry.OpClear, wreq, res, ferr, start)
	return res, ferr
}
