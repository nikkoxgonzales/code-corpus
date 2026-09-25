// Package xerr defines the error type every command returns. Each error carries
// an exit code and a one-line hint telling the agent what to run next.
package xerr

import (
	"errors"
	"fmt"
)

// Exit codes, stable across versions.
const (
	OK       = 0
	User     = 1 // bad input; the hint says how to fix it
	NotFound = 2 // repo, file or symbol does not exist
	Network  = 3 // git or HTTP failure
	Partial  = 4 // some items succeeded, some failed
)

type Error struct {
	Code int    `json:"code"`
	Msg  string `json:"message"`
	Hint string `json:"hint,omitempty"`
}

func (e *Error) Error() string { return e.Msg }

func New(code int, hint, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...), Hint: hint}
}

// As converts any error into an *Error, defaulting to code User.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: User, Msg: err.Error()}
}
