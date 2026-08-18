package sessionstore

import (
	"crypto/rand"
	"encoding/hex"
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

func readUint64File(root *os.Root, name string) (uint64, bool, error) {
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
	if _, err := writeAndClose(temporary, data, sync); err != nil {
		return fmt.Errorf("persist state snapshot: %w", err)
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

func writeAndClose(file *os.File, data []byte, sync bool) (bool, error) {
	if _, err := file.Write(data); err != nil {
		return false, errors.Join(err, file.Close())
	}
	if sync {
		if err := file.Sync(); err != nil {
			return false, errors.Join(err, file.Close())
		}
	}
	return true, file.Close()
}

func newSnapshotTemporaryName(name string) (string, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(name), "."+filepath.Base(name)+"-"+hex.EncodeToString(suffix[:])), nil
}
