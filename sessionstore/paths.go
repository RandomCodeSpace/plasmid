package sessionstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	directoryMode os.FileMode = 0o700
	fileMode      os.FileMode = 0o600
	maxSegmentLen             = 200
)

// paths owns the evaluated storage directory and its confinement handle.
// Relative names returned by its methods are safe to pass to root methods.
type paths struct {
	dir  string
	root *os.Root
}

func openPaths(dir string) (*paths, error) {
	if dir == "" {
		return nil, fmt.Errorf("open session storage: %w", ErrInvalidID)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("make storage path absolute: %w", err)
	}
	if err := os.MkdirAll(abs, directoryMode); err != nil {
		return nil, fmt.Errorf("create storage directory: %w", err)
	}
	if err := os.Chmod(abs, directoryMode); err != nil {
		return nil, fmt.Errorf("set storage directory permissions: %w", err)
	}
	evaluated, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("evaluate storage directory: %w", err)
	}
	root, err := os.OpenRoot(evaluated)
	if err != nil {
		return nil, fmt.Errorf("open storage root: %w", err)
	}
	return &paths{dir: evaluated, root: root}, nil
}

func (p *paths) close() error {
	if p == nil || p.root == nil {
		return nil
	}
	return p.root.Close()
}

func (p *paths) appState(app string) (string, error) {
	app, err := encodeSegment(app)
	if err != nil {
		return "", err
	}
	return filepath.Join("apps", app, "app_state.json"), nil
}

func (p *paths) appSequence(app string) (string, error) {
	app, err := encodeSegment(app)
	if err != nil {
		return "", err
	}
	return filepath.Join("apps", app, "shared_sequence"), nil
}

func (p *paths) appJournal(app string) (string, error) {
	app, err := encodeSegment(app)
	if err != nil {
		return "", err
	}
	return filepath.Join("apps", app, "app_state.jsonl"), nil
}

func (p *paths) userState(app, user string) (string, error) {
	app, user, err := encodeIdentity(app, user)
	if err != nil {
		return "", err
	}
	return filepath.Join("apps", app, "users", user, "user_state.json"), nil
}

func (p *paths) userJournal(app, user string) (string, error) {
	app, user, err := encodeIdentity(app, user)
	if err != nil {
		return "", err
	}
	return filepath.Join("apps", app, "users", user, "user_state.jsonl"), nil
}

func (p *paths) sessionLog(app, user, session string) (string, error) {
	app, user, err := encodeIdentity(app, user)
	if err != nil {
		return "", err
	}
	session, err = encodeSegment(session)
	if err != nil {
		return "", err
	}
	return filepath.Join("apps", app, "users", user, "sessions", session+".jsonl"), nil
}

func (p *paths) sessionDir(app, user string) (string, error) {
	app, user, err := encodeIdentity(app, user)
	if err != nil {
		return "", err
	}
	return filepath.Join("apps", app, "users", user, "sessions"), nil
}

func (p *paths) ensureParent(name string) error {
	parent := filepath.Dir(name)
	if parent == "." {
		return nil
	}
	if err := p.root.MkdirAll(parent, directoryMode); err != nil {
		return fmt.Errorf("create session storage parent: %w", err)
	}
	for current := parent; current != "."; current = filepath.Dir(current) {
		if err := p.root.Chmod(current, directoryMode); err != nil {
			return fmt.Errorf("set session storage parent permissions: %w", err)
		}
	}
	return nil
}

func encodeIdentity(app, user string) (string, string, error) {
	encodedApp, err := encodeSegment(app)
	if err != nil {
		return "", "", err
	}
	encodedUser, err := encodeSegment(user)
	if err != nil {
		return "", "", err
	}
	return encodedApp, encodedUser, nil
}

// encodeSegment returns the only valid on-disk spelling for an identifier.
func encodeSegment(value string) (string, error) {
	if value == "" {
		return "", ErrInvalidID
	}

	encoded := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		b := value[i]
		if isUnreserved(b) && !(len(value) == 1 && b == '.') && !(len(value) == 2 && value == "..") {
			encoded = append(encoded, b)
			continue
		}
		encoded = append(encoded, '%', hexUpper(b>>4), hexUpper(b&0x0f))
	}
	if len(encoded) > maxSegmentLen {
		return "", ErrInvalidID
	}
	return string(encoded), nil
}

// decodeSegment accepts only canonical encoded identifiers. It intentionally
// permits decoded separators and NUL bytes: the encoded name and os.Root are
// the containment boundary, not the decoded value.
func decodeSegment(encoded string) (string, error) {
	if encoded == "" || len(encoded) > maxSegmentLen {
		return "", ErrInvalidID
	}

	decoded := make([]byte, 0, len(encoded))
	for i := 0; i < len(encoded); {
		b := encoded[i]
		switch {
		case isUnreserved(b):
			decoded = append(decoded, b)
			i++
		case b == '%' && i+2 < len(encoded):
			hi, okHi := fromUpperHex(encoded[i+1])
			lo, okLo := fromUpperHex(encoded[i+2])
			if !okHi || !okLo {
				return "", ErrInvalidID
			}
			decoded = append(decoded, hi<<4|lo)
			i += 3
		default:
			return "", ErrInvalidID
		}
	}

	canonical, err := encodeSegment(string(decoded))
	if err != nil || canonical != encoded {
		return "", ErrInvalidID
	}
	return string(decoded), nil
}

func isUnreserved(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '.' || b == '_' || b == '-'
}

func hexUpper(value byte) byte {
	if value < 10 {
		return '0' + value
	}
	return 'A' + value - 10
}

func fromUpperHex(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
