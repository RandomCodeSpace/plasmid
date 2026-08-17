// Package sessionstore persists framework-free loop sessions as JSONL files.
package sessionstore

import "errors"

var (
	// ErrSessionNotFound reports an absent session log.
	ErrSessionNotFound = errors.New("sessionstore: session not found")
	// ErrSessionExists reports an attempt to create an existing session.
	ErrSessionExists = errors.New("sessionstore: session exists")
	// ErrCorruptLog reports a complete invalid JSONL record.
	ErrCorruptLog = errors.New("sessionstore: corrupt log")
	// ErrInvalidID reports an invalid app, user, or session identifier.
	ErrInvalidID = errors.New("sessionstore: invalid identifier")
	// ErrInvalidEvent reports an event that cannot be persisted.
	ErrInvalidEvent = errors.New("sessionstore: invalid event")
	// ErrClosed reports an operation on a closed store.
	ErrClosed = errors.New("sessionstore: closed")
)
