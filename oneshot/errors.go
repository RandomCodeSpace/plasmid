package oneshot

import (
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
	if cause == nil {
		cause = sentinel
	} else if sentinel != nil && !errors.Is(cause, sentinel) {
		cause = errors.Join(sentinel, cause)
	}
	return &Error{Code: code, Op: op, Err: cause}
}
