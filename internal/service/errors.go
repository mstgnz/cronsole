// Package service holds the business rules.
//
// Rules live here rather than in a handler because a handler can be bypassed
// by the next route somebody adds. The API, the interface and the sync
// endpoint all reach the same methods, so a rule written once applies to all
// three.
package service

import "errors"

// Sentinel errors. Handlers map these to status codes; nothing outside this
// package inspects error strings.
var (
	ErrNotFound      = errors.New("not found")
	ErrForbidden     = errors.New("forbidden")
	ErrConflict      = errors.New("already exists")
	ErrValidation    = errors.New("validation failed")
	ErrInvalidLogin  = errors.New("invalid email or password")
	ErrInactiveUser  = errors.New("account is not active")
	ErrChainLoop     = errors.New("this link would create a loop")
	ErrSelfLink      = errors.New("a job cannot trigger itself")
	ErrProjectLocked = errors.New("project still has jobs")
)

// FieldError is a validation failure the interface can render next to the
// field that caused it.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError carries every failure at once.
//
// All of them, not the first: a form that reports one problem per submission
// takes as many round trips as it has mistakes, and the person filling it in
// learns nothing about the shape of the thing.
type ValidationError struct {
	Errors []FieldError `json:"errors"`
}

func (v *ValidationError) Error() string {
	if len(v.Errors) == 0 {
		return "validation failed"
	}
	out := v.Errors[0].Field + ": " + v.Errors[0].Message
	if len(v.Errors) > 1 {
		out += " (and " + itoa(len(v.Errors)-1) + " more)"
	}
	return out
}

// Unwrap lets errors.Is(err, ErrValidation) work on a ValidationError, so a
// handler can branch on the class without knowing the type.
func (v *ValidationError) Unwrap() error { return ErrValidation }

// Add records a failure and returns the receiver so checks can be chained.
func (v *ValidationError) Add(field, message string) *ValidationError {
	v.Errors = append(v.Errors, FieldError{Field: field, Message: message})
	return v
}

// OK reports whether nothing failed.
func (v *ValidationError) OK() bool { return len(v.Errors) == 0 }

// ErrOrNil returns nil when nothing failed, so a caller can `return v.ErrOrNil()`
// without repeating the emptiness check.
func (v *ValidationError) ErrOrNil() error {
	if v.OK() {
		return nil
	}
	return v
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
