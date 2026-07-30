package cli

import (
	"errors"

	"github.com/demirtechcom/iold/internal/catalog"
	"github.com/demirtechcom/iold/internal/doctor"
	"github.com/demirtechcom/iold/internal/hf"
	"github.com/demirtechcom/iold/internal/state"
)

// Stable exit codes for automation (docs/TESTING.md: CLI parsing and stable
// exit codes). These are part of the CLI contract; only append.
const (
	ExitOK             = 0
	ExitFailure        = 1 // unclassified error
	ExitUsage          = 2 // bad arguments or unknown command
	ExitNotFound       = 3 // unknown catalog model or deployment
	ExitConflict       = 4 // illegal state transition or concurrent change
	ExitEnvironment    = 5 // doctor checks failed
	ExitNotImplemented = 6 // command not available yet
)

// ErrUsage marks command-line usage errors; wrap with fmt.Errorf("%w: ...").
var ErrUsage = errors.New("usage error")

// ExitCode maps an error returned by Run to its stable process exit code.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrUsage):
		return ExitUsage
	case errors.Is(err, catalog.ErrUnknownModel), errors.Is(err, state.ErrNotFound),
		errors.Is(err, hf.ErrNotFound):
		return ExitNotFound
	case errors.Is(err, state.ErrIllegalTransition), errors.Is(err, state.ErrConflict),
		errors.Is(err, state.ErrDuplicate), errors.Is(err, state.ErrNotDestroyed):
		return ExitConflict
	case errors.Is(err, doctor.ErrChecksFailed), errors.Is(err, hf.ErrAuthRequired),
		errors.Is(err, hf.ErrNoRevision):
		return ExitEnvironment
	case errors.Is(err, ErrNotImplemented):
		return ExitNotImplemented
	default:
		return ExitFailure
	}
}
