package sessionstore

import (
	"context"
	"encoding/json"
	"fmt"
)

func (s *Store) AppendSidecar(ctx context.Context, app, user, id, kind string, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.open(); err != nil {
		return err
	}
	if kind == "" {
		return ErrInvalidID
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode sidecar: %w", err)
	}
	name, err := s.identityPath(app, user, id)
	if err != nil {
		return err
	}
	prepared, err := prepareSidecarRecord(sidecar{Kind: kind, Data: data})
	if err != nil {
		return err
	}
	lock, err := s.appLock(app)
	if err != nil {
		return err
	}
	lock.Lock()
	defer lock.Unlock()
	scan, err := s.scanAppLocked(app)
	if err != nil {
		return err
	}
	log := scan.Logs[name]
	if log == nil {
		return ErrSessionNotFound
	}
	if err := log.appendBytes(s.paths, prepared.bytes(0), s.fsync); err != nil {
		return err
	}
	log.sidecars[kind] = append([]byte(nil), data...)
	return nil
}

func (s *Store) LoadSidecar(ctx context.Context, app, user, id, kind string, destination any) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := s.open(); err != nil {
		return false, err
	}
	if kind == "" {
		return false, ErrInvalidID
	}
	name, err := s.identityPath(app, user, id)
	if err != nil {
		return false, err
	}
	lock, err := s.appLock(app)
	if err != nil {
		return false, err
	}
	lock.Lock()
	defer lock.Unlock()
	scan, err := s.scanAppLocked(app)
	if err != nil {
		return false, err
	}
	log := scan.Logs[name]
	if log == nil {
		return false, ErrSessionNotFound
	}
	data, exists := log.sidecars[kind]
	if !exists {
		return false, nil
	}
	if destination == nil {
		return false, fmt.Errorf("decode sidecar: nil destination")
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return false, fmt.Errorf("decode sidecar: %w", err)
	}
	return true, nil
}
