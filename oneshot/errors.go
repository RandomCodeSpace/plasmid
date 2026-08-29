package oneshot

import (
	"context"
	"errors"
	"fmt"
)

// ErrorCode is a stable machine-readable one-shot failure category.
type ErrorCode string

const (
	CodeInvalidArgument ErrorCode = "invalid_argument"
	CodeCanceled        ErrorCode = "canceled"
	CodeModelPanic      ErrorCode = "model_panic"
	CodeToolPanic       ErrorCode = "tool_panic"
	CodeNoFinalResponse ErrorCode = "no_final_response"
	CodeExecutionFailed ErrorCode = "execution_failed"
	CodeCleanupFailed   ErrorCode = "cleanup_failed"
)

var (
	ErrInvalidArgument = errors.New("oneshot: invalid argument")
	ErrCanceled        = errors.New("oneshot: canceled")
	ErrModelPanic      = errors.New("oneshot: caller model panicked")
	ErrToolPanic       = errors.New("oneshot: caller tool panicked")
	ErrNoFinalResponse = errors.New("oneshot: no final response")
	ErrExecutionFailed = errors.New("oneshot: execution failed")
	ErrCleanupFailed   = errors.New("oneshot: cleanup failed")
)

// Error carries a stable code without exposing recovered panic values.
type Error struct {
	Code ErrorCode
	Op   string
	Err  error
}

type internalError struct {
	value *Error
}

func (e *internalError) Error() string { return e.value.Error() }
func (e *internalError) Unwrap() error { return e.value }

type callerBoundaryError struct {
	cause error
}

func (*callerBoundaryError) Error() string { return "caller operation failed" }
func (e *callerBoundaryError) Is(target error) bool {
	return errors.Is(e.cause, target)
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Op == "" {
		return fmt.Sprintf("oneshot %s: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("oneshot %s: %s: %v", e.Code, e.Op, e.Err)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// CodeOf returns the first one-shot error code in err's unwrap tree.
func CodeOf(err error) ErrorCode {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}

func codedError(code ErrorCode, op string, sentinel, cause error) error {
	safeCause := sentinel
	if code == CodeCanceled {
		switch {
		case errors.Is(cause, context.DeadlineExceeded):
			safeCause = errors.Join(sentinel, context.DeadlineExceeded)
		case errors.Is(cause, context.Canceled):
			safeCause = errors.Join(sentinel, context.Canceled)
		}
	}
	return &internalError{value: &Error{Code: code, Op: op, Err: safeCause}}
}

func untrustedCallerError(cause error) error {
	if cause == nil {
		return nil
	}
	return &callerBoundaryError{cause: cause}
}
