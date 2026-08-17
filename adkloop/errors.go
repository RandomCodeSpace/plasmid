package adkloop

import "errors"

var (
	// ErrNotConfigured is yielded when Run is consumed before Configure succeeds.
	ErrNotConfigured = errors.New("adkloop: provider is not configured")
	// ErrAlreadyConfigured is returned after the single successful Configure call.
	ErrAlreadyConfigured = errors.New("adkloop: provider is already configured")
	// ErrClosed is returned or yielded after Close.
	ErrClosed = errors.New("adkloop: provider is closed")
	// ErrInvalidConfig identifies invalid provider or runner configuration.
	ErrInvalidConfig = errors.New("adkloop: invalid configuration")
	// ErrFidelity identifies a conversion that would discard provider data.
	ErrFidelity = errors.New("adkloop: conversion would lose provider data")
	// ErrForeignSession identifies an ADK session not owned by this adapter.
	ErrForeignSession = errors.New("adkloop: foreign session")
	// ErrImmutableHookField identifies hook mutations ADK cannot apply safely.
	ErrImmutableHookField = errors.New("adkloop: hook mutated immutable identity")
)
