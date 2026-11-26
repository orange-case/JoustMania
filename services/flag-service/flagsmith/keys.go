package flagsmith

import (
	"strings"

	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

// ScopeStack is Key.Scope of a stack value (the feature's environment value).
const ScopeStack = "stack"

// Key names one synced value: a flag in one scope. Scope is ScopeStack or the
// identity the scope maps to (architecture note §2): game:<id>,
// game:<id>:<serial>, experiment:<id>:<arm>, real.
type Key struct {
	Set, Flag, Scope string
}

// KeyOf maps a flag and a scope (registry scope name and its properties) to a
// Key.
func KeyOf(set, flag, scopeName string, sc state.Scope) Key {
	k := Key{Set: set, Flag: flag, Scope: ScopeStack}
	switch scopeName {
	case registry.ScopeGame:
		k.Scope = "game:" + sc.GameID
	case registry.ScopeGameSerial:
		k.Scope = "game:" + sc.GameID + ":" + sc.Serial
	case registry.ScopeExperiment:
		k.Scope = "experiment:" + sc.ExperimentID + ":" + sc.Arm
	case registry.ScopeReal:
		k.Scope = "real"
	}
	return k
}

// IdentityOf returns the identity identifier a Key's value lives on, or ""
// for a stack value.
func IdentityOf(k Key) string {
	if k.Scope == ScopeStack {
		return ""
	}
	return k.Scope
}

// GameOfIdentity returns the game id of a game:<id> or game:<id>:<serial>
// identifier, else "". Game ids never hold ":" in practice; a serial may.
func GameOfIdentity(identifier string) string {
	rest, ok := strings.CutPrefix(identifier, "game:")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, ":")
	return id
}

// ScopeOfIdentity is the inverse of KeyOf for an identity identifier: the
// scope properties and the registry scope name, ok false for an identifier
// the link does not use.
func ScopeOfIdentity(identifier string) (state.Scope, string, bool) {
	if identifier == "real" {
		return state.Scope{GameKind: "real"}, registry.ScopeReal, true
	}
	if rest, ok := strings.CutPrefix(identifier, "game:"); ok && rest != "" {
		id, serial, hasSerial := strings.Cut(rest, ":")
		switch {
		case id == "":
			return state.Scope{}, "", false
		case !hasSerial:
			return state.Scope{GameID: id}, registry.ScopeGame, true
		case serial != "":
			return state.Scope{GameID: id, Serial: serial}, registry.ScopeGameSerial, true
		}
		return state.Scope{}, "", false
	}
	if rest, ok := strings.CutPrefix(identifier, "experiment:"); ok {
		id, arm, found := strings.Cut(rest, ":")
		if found && id != "" && arm != "" {
			return state.Scope{ExperimentID: id, Arm: arm}, registry.ScopeExperiment, true
		}
	}
	return state.Scope{}, "", false
}
