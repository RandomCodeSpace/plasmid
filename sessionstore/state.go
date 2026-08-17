package sessionstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	appStatePrefix  = "app:"
	userStatePrefix = "user:"
	tempStatePrefix = "temp:"
)

// splitState separates state by its durable scope. Temporary values are never
// returned, so callers cannot accidentally commit them.
func splitState(state map[string]any) (session, app, user map[string]any) {
	session = make(map[string]any)
	app = make(map[string]any)
	user = make(map[string]any)
	for key, value := range state {
		switch {
		case strings.HasPrefix(key, tempStatePrefix):
			continue
		case strings.HasPrefix(key, appStatePrefix):
			app[strings.TrimPrefix(key, appStatePrefix)] = value
		case strings.HasPrefix(key, userStatePrefix):
			user[strings.TrimPrefix(key, userStatePrefix)] = value
		default:
			session[key] = value
		}
	}
	return session, app, user
}

type stateCheckpoint struct {
	State   map[string]any
	Through uint64
	Exists  bool
}

func readStateCheckpoint(root *os.Root, stateName, orderName string) (stateCheckpoint, error) {
	if err := validateSnapshotName(stateName); err != nil {
		return stateCheckpoint{}, err
	}
	data, err := root.ReadFile(stateName)
	if errors.Is(err, fs.ErrNotExist) {
		return stateCheckpoint{State: map[string]any{}}, nil
	}
	if err != nil {
		return stateCheckpoint{}, fmt.Errorf("read state snapshot: %w", err)
	}
	state := map[string]any{}
	if err := json.Unmarshal(data, &state); err != nil {
		return stateCheckpoint{}, fmt.Errorf("decode state snapshot: %w", err)
	}
	through, _, err := readUint64File(root, orderName)
	if err != nil {
		return stateCheckpoint{}, err
	}
	return stateCheckpoint{State: state, Through: through, Exists: true}, nil
}

func writeStateCheckpoint(root *os.Root, stateName, orderName string, value stateCheckpoint, sync bool) error {
	if err := writeStateSnapshot(root, stateName, value.State, sync); err != nil {
		return err
	}
	return writeUint64File(root, orderName, value.Through, sync)
}

func readUint64File(root *os.Root, name string) (uint64, bool, error) {
	if err := validateSnapshotName(name); err != nil {
		return 0, false, err
	}
	data, err := root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read state order: %w", err)
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("decode state order: %w", err)
	}
	return value, true, nil
}

func writeUint64File(root *os.Root, name string, value uint64, sync bool) error {
	if err := validateSnapshotName(name); err != nil {
		return err
	}
	return writeFileAtomic(root, name, []byte(strconv.FormatUint(value, 10)+"\n"), sync)
}

func withoutTemporaryState(state map[string]any) map[string]any {
	if state == nil {
		return nil
	}
	filtered := make(map[string]any, len(state))
	for key, value := range state {
		if !strings.HasPrefix(key, tempStatePrefix) {
			filtered[key] = value
		}
	}
	return filtered
}

// mergeState reconstructs the public state view without allowing shared state
// to overwrite session-local keys through an unprefixed alias.
func mergeState(session, app, user map[string]any) map[string]any {
	merged := maps.Clone(session)
	if merged == nil {
		merged = make(map[string]any)
	}
	for key, value := range app {
		merged[appStatePrefix+key] = value
	}
	for key, value := range user {
		merged[userStatePrefix+key] = value
	}
	return merged
}

// readStateSnapshot treats a missing snapshot as empty state. name must be a
// canonical relative name beneath root.
func readStateSnapshot(root *os.Root, name string) (map[string]any, error) {
	if err := validateSnapshotName(name); err != nil {
		return nil, err
	}
	data, err := root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state snapshot: %w", err)
	}
	state := make(map[string]any)
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode state snapshot: %w", err)
	}
	return state, nil
}

// writeStateSnapshot makes a state replacement durable without relying on
// Store internals. name must be a canonical relative name beneath root.
// Callers choose whether durability barriers are enabled.
func writeStateSnapshot(root *os.Root, name string, state map[string]any, sync bool) error {
	if err := validateSnapshotName(name); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode state snapshot: %w", err)
	}
	return writeFileAtomic(root, name, data, sync)
}

func writeFileAtomic(root *os.Root, name string, data []byte, sync bool) error {
	temporaryName, err := newSnapshotTemporaryName(name)
	if err != nil {
		return fmt.Errorf("create state snapshot: %w", err)
	}
	temporary, err := root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("create state snapshot: %w", err)
	}
	defer root.Remove(temporaryName)
	if err := root.Chmod(temporaryName, fileMode); err != nil {
		temporary.Close()
		return fmt.Errorf("set state snapshot mode: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write state snapshot: %w", err)
	}
	if sync {
		if err := temporary.Sync(); err != nil {
			temporary.Close()
			return fmt.Errorf("sync state snapshot: %w", err)
		}
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close state snapshot: %w", err)
	}
	if err := root.Rename(temporaryName, name); err != nil {
		return fmt.Errorf("replace state snapshot: %w", err)
	}
	if !sync {
		return nil
	}
	dir, err := root.Open(filepath.Dir(name))
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func validateSnapshotName(name string) error {
	if name == "" || name == "." || filepath.IsAbs(name) || filepath.Clean(name) != name {
		return fmt.Errorf("invalid state snapshot name %q", name)
	}
	return nil
}

func newSnapshotTemporaryName(name string) (string, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(name), "."+filepath.Base(name)+"-"+hex.EncodeToString(suffix[:])), nil
}
