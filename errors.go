package plasmid

import (
	"errors"
	"fmt"
)

// ErrorCode is a stable machine-readable Plasmid failure category.
type ErrorCode string

const (
	CodeInvalidArgument    ErrorCode = "invalid_argument"
	CodeConstructionFailed ErrorCode = "construction_failed"
	CodeUnknownSession     ErrorCode = "unknown_session"
	CodeSessionBusy        ErrorCode = "session_busy"
	CodeDuplicate          ErrorCode = "duplicate"
	CodeClosed             ErrorCode = "closed"
	CodeRegistrationSealed ErrorCode = "registration_sealed"
	CodeNoFinalResponse    ErrorCode = "no_final_response"
	CodeRuntimeFailed      ErrorCode = "runtime_failed"
	CodeCloseFailed        ErrorCode = "close_failed"
)

var (
	ErrInvalidArgument    = errors.New("plasmid: invalid argument")
	ErrConstructionFailed = errors.New("plasmid: construction failed")
	ErrUnknownSession     = errors.New("plasmid: unknown session")
	ErrSessionBusy        = errors.New("plasmid: session busy")
	ErrDuplicate          = errors.New("plasmid: duplicate registration")
	ErrClosed             = errors.New("plasmid: closed")
	ErrRegistrationSealed = errors.New("plasmid: registration sealed")
	ErrNoFinalResponse    = errors.New("plasmid: no final response")
	ErrRuntimeFailed      = errors.New("plasmid: runtime failed")
	ErrCloseTimeout       = errors.New("plasmid: close timeout")
	ErrCloseFailed        = errors.New("plasmid: close failed")
)

// Error carries a stable code while retaining the original cause.
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
		return fmt.Sprintf("plasmid %s: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("plasmid %s: %s: %v", e.Code, e.Op, e.Err)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// CodeOf returns the first Plasmid error code in err's unwrap tree.
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
