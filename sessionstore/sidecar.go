package sessionstore

import (
	"context"
	"encoding/json"
	"fmt"
)

func (s *Store) AppendSidecar(ctx context.Context, app, user, id, kind string, value any) error {
	if kind == "" {
		return ErrInvalidID
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.begin(); err != nil {
		return err
	}
	var notices warningBuffer
	defer s.emitWarnings(&notices)
	defer s.end()
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode sidecar: %w", err)
	}
	locks, _, name, err := s.locksFor(app, user, id)
	if err != nil {
		return err
	}
	locks.op.Lock()
	defer locks.op.Unlock()
	locks.io.Lock()
	defer locks.io.Unlock()
	log, err := loadSessionLog(s.paths, name, app, user, id, s.fsync, &notices)
	if err != nil {
		return err
	}
	line := normalizedRecordLine(record{V: recordVersion, Type: recordSidecar, Sidecar: &sidecar{Kind: kind, Data: data}})
	_, err = log.appendBytes(s.paths, line, s.fsync, nil)
	return err
}

func (s *Store) LoadSidecar(ctx context.Context, app, user, id, kind string, destination any) (bool, error) {
	if kind == "" {
		return false, ErrInvalidID
	}
	if destination == nil {
		return false, fmt.Errorf("decode sidecar: nil destination")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := s.begin(); err != nil {
		return false, err
	}
	var notices warningBuffer
	defer s.emitWarnings(&notices)
	defer s.end()
	locks, _, name, err := s.locksFor(app, user, id)
	if err != nil {
		return false, err
	}
	locks.op.Lock()
	defer locks.op.Unlock()
	locks.io.Lock()
	log, err := loadSessionLog(s.paths, name, app, user, id, s.fsync, &notices)
	locks.io.Unlock()
	if err != nil {
		return false, err
	}
	data, exists := log.sidecars[kind]
	if !exists {
		return false, nil
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return false, fmt.Errorf("decode sidecar: %w", err)
	}
	return true, nil
}
