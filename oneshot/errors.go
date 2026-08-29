package oneshot

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

// ErrorCode is a stable machine-readable one-shot failure category.
type ErrorCode string

const (
	CodeInvalidArgument        ErrorCode = "invalid_argument"
	CodeCanceled               ErrorCode = "canceled"
	CodeModelPanic             ErrorCode = "model_panic"
	CodeToolPanic              ErrorCode = "tool_panic"
	CodeOutputTruncated        ErrorCode = "output_truncated"
	CodeTextTruncated          ErrorCode = "text_truncated"
	CodeModelCallLimit         ErrorCode = "model_call_limit"
	CodeToolCallLimit          ErrorCode = "tool_call_limit"
	CodeToolCallingUnsupported ErrorCode = "tool_calling_unsupported"
	CodeNoFinalResponse        ErrorCode = "no_final_response"
	CodeExecutionFailed        ErrorCode = "execution_failed"
	CodeCleanupFailed          ErrorCode = "cleanup_failed"
)

var (
	ErrInvalidArgument        = errors.New("oneshot: invalid argument")
	ErrCanceled               = errors.New("oneshot: canceled")
	ErrModelPanic             = errors.New("oneshot: caller model panicked")
	ErrToolPanic              = errors.New("oneshot: caller tool panicked")
	ErrOutputTruncated        = errors.New("oneshot: model output truncated")
	ErrTextTruncated          = errors.New("oneshot: returned text truncated")
	ErrModelCallLimit         = errors.New("oneshot: model call limit reached")
	ErrToolCallLimit          = errors.New("oneshot: tool call limit exceeded")
	ErrToolCallingUnsupported = errors.New("oneshot: tool calling unsupported")
	ErrNoFinalResponse        = errors.New("oneshot: no final response")
	ErrExecutionFailed        = errors.New("oneshot: execution failed")
	ErrCleanupFailed          = errors.New("oneshot: cleanup failed")
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
	matchesCanceled bool
	matchesDeadline bool
}

func (*callerBoundaryError) Error() string { return "caller operation failed" }

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

func untrustedCallerError(cause error) (safe error, panicked bool) {
	if cause == nil {
		return nil, false
	}
	matchesCanceled, matchesDeadline, panicked := inspectUntrustedContextErrors(cause)
	if panicked {
		return nil, true
	}
	return &callerBoundaryError{
		matchesCanceled: matchesCanceled,
		matchesDeadline: matchesDeadline,
	}, false
}

const maxUntrustedErrorVisits = 64

type untrustedErrorVisit struct {
	typeValue reflect.Type
	pointer   uintptr
	value     error
}

func inspectUntrustedContextErrors(cause error) (matchesCanceled, matchesDeadline, panicked bool) {
	pending := []error{cause}
	visited := make(map[untrustedErrorVisit]struct{})
	for visits := 0; len(pending) != 0 && visits < maxUntrustedErrorVisits; visits++ {
		last := len(pending) - 1
		current := pending[last]
		pending = pending[:last]
		if current == nil {
			continue
		}
		if key, ok := untrustedErrorIdentity(current); ok {
			if _, seen := visited[key]; seen {
				continue
			}
			visited[key] = struct{}{}
		}

		if callUntrustedError(current) {
			return false, false, true
		}
		matchesCanceled = matchesCanceled || sameErrorIdentity(current, context.Canceled)
		matchesDeadline = matchesDeadline || sameErrorIdentity(current, context.DeadlineExceeded)
		switch wrapped := current.(type) {
		case joinedErrorUnwrapper:
			children, callerPanicked := callUntrustedJoinedUnwrap(wrapped)
			if callerPanicked {
				return false, false, true
			}
			for _, child := range children {
				if len(pending) >= maxUntrustedErrorVisits {
					break
				}
				pending = append(pending, child)
			}
		case errorUnwrapper:
			child, callerPanicked := callUntrustedUnwrap(wrapped)
			if callerPanicked {
				return false, false, true
			}
			if len(pending) < maxUntrustedErrorVisits {
				pending = append(pending, child)
			}
		}
	}
	return matchesCanceled, matchesDeadline, false
}

func callUntrustedError(value error) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	_ = value.Error()
	return false
}

func callUntrustedUnwrap(value errorUnwrapper) (result error, panicked bool) {
	defer func() {
		if recover() != nil {
			result = nil
			panicked = true
		}
	}()
	return value.Unwrap(), false
}

func callUntrustedJoinedUnwrap(value joinedErrorUnwrapper) (result []error, panicked bool) {
	defer func() {
		if recover() != nil {
			result = nil
			panicked = true
		}
	}()
	return value.Unwrap(), false
}

func untrustedErrorIdentity(value error) (untrustedErrorVisit, bool) {
	typeValue := reflect.TypeOf(value)
	reflected := reflect.ValueOf(value)
	if reflected.Comparable() {
		return untrustedErrorVisit{typeValue: typeValue, value: value}, true
	}
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return untrustedErrorVisit{typeValue: typeValue, pointer: reflected.Pointer()}, true
	default:
		return untrustedErrorVisit{}, false
	}
}

func sameErrorIdentity(value, target error) bool {
	valueType := reflect.TypeOf(value)
	if valueType == nil || valueType != reflect.TypeOf(target) || !valueType.Comparable() {
		return false
	}
	return value == target
}
