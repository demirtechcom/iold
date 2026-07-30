// Package state persists deployment ownership state in a local SQLite
// database (docs/ARCHITECTURE.md §5 state store, §6 deployment state machine).
package state

import (
	"errors"
	"fmt"
	"slices"
)

type Phase string

const (
	PhaseRequested           Phase = "REQUESTED"
	PhaseValidating          Phase = "VALIDATING"
	PhaseDownloading         Phase = "DOWNLOADING"
	PhaseStarting            Phase = "STARTING"
	PhaseHealthy             Phase = "HEALTHY"
	PhaseRegistering         Phase = "REGISTERING"
	PhaseReady               Phase = "READY"
	PhaseUnregisteredHealthy Phase = "UNREGISTERED_HEALTHY"
	PhaseFailed              Phase = "FAILED"
	PhaseCrashed             Phase = "CRASHED"
	PhaseDestroying          Phase = "DESTROYING"
	PhaseDestroyed           Phase = "DESTROYED"
)

var ErrIllegalTransition = errors.New("illegal state transition")

var transitions = map[Phase][]Phase{
	PhaseRequested:   {PhaseValidating},
	PhaseValidating:  {PhaseDownloading},
	PhaseDownloading: {PhaseStarting},
	PhaseStarting:    {PhaseHealthy},
	PhaseHealthy:     {PhaseRegistering},
	PhaseRegistering: {PhaseReady, PhaseUnregisteredHealthy},
	// Registration reconciliation retries from the split state.
	PhaseUnregisteredHealthy: {PhaseRegistering, PhaseDestroying},
	PhaseReady:               {PhaseDestroying},
	PhaseFailed:              {PhaseDestroying},
	PhaseCrashed:             {PhaseDestroying},
	PhaseDestroying:          {PhaseDestroyed},
	PhaseDestroyed:           {},
}

// CanTransition reports whether from -> to is a legal edge. Any phase
// except the terminal DESTROYED may fail.
func CanTransition(from, to Phase) bool {
	if to == PhaseFailed {
		return from != PhaseDestroyed && from != PhaseFailed && from != PhaseCrashed
	}
	// CRASHED is the reconciliation outcome for any interrupted active
	// deployment. Destruction has its own state and is never rewritten by
	// a status probe.
	if to == PhaseCrashed {
		return from != PhaseDestroyed && from != PhaseDestroying &&
			from != PhaseFailed && from != PhaseCrashed
	}
	return slices.Contains(transitions[from], to)
}

func checkTransition(from, to Phase) error {
	if _, known := transitions[from]; !known {
		return fmt.Errorf("%w: unknown phase %q", ErrIllegalTransition, from)
	}
	if _, known := transitions[to]; !known {
		return fmt.Errorf("%w: unknown phase %q", ErrIllegalTransition, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, from, to)
	}
	return nil
}
