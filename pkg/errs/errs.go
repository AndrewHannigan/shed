// Package errs defines the stable exit codes used by shed and a
// typed error that carries one. main.go converts these to process exit
// status.
package errs

import (
	"errors"
	"fmt"
)

// Exit codes are part of shed's CLI contract: help text documents them and
// scripts/hooks match on them, so the values are stable — never renumber.
const (
	OK       = 0
	NotFound = 2
	Exists   = 3
	Dirty    = 4
	Locked   = 5
	Network  = 6
	Config   = 7

	// MissingDep means a required external program (git) is not on PATH.
	MissingDep = 8
)

// Coded wraps an error with a specific exit code. main.go inspects this
// type to choose the process exit status.
type Coded struct {
	Code int
	Err  error
}

func (c *Coded) Error() string { return c.Err.Error() }
func (c *Coded) Unwrap() error { return c.Err }

// New constructs a Coded error from a code and a formatted message.
func New(code int, format string, a ...any) *Coded {
	return &Coded{Code: code, Err: fmt.Errorf(format, a...)}
}

// Wrap attaches a code to an existing error. Returns nil if err is nil
// so callers can pass through chained error-returning calls.
func Wrap(code int, err error) error {
	if err == nil {
		return nil
	}
	return &Coded{Code: code, Err: err}
}

// EnsureCoded guarantees err carries an exit code without clobbering one it
// already has: if err is already a *Coded it is returned unchanged, otherwise
// it is wrapped with code. Returns nil if err is nil.
func EnsureCoded(err error, code int) error {
	var c *Coded
	if errors.As(err, &c) {
		return err
	}
	return Wrap(code, err)
}
