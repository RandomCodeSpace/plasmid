package sessionstore

import (
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

type sharedStatePaths struct {
	appJournal   string
	userJournal  string
	appSnapshot  string
	userSnapshot string
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
	return p.root.Close()
}

func (p *paths) appSequence(app string) string {
	return filepath.Join("apps", encodePathSegment(app), "shared_sequence")
}

func (p *paths) userJournal(app, user string) string {
	return filepath.Join("apps", encodePathSegment(app), "users", encodePathSegment(user), "user_state.jsonl")
}

func (p *paths) sharedState(app, user string) sharedStatePaths {
	app, user = encodePathSegment(app), encodePathSegment(user)
	base := filepath.Join("apps", app)
	userBase := filepath.Join(base, "users", user)
	return sharedStatePaths{
		appJournal: filepath.Join(base, "app_state.jsonl"), userJournal: filepath.Join(userBase, "user_state.jsonl"),
		appSnapshot: filepath.Join(base, "app_state.json"), userSnapshot: filepath.Join(userBase, "user_state.json"),
	}
}

func (p *paths) sessionLog(app, user, session string) string {
	return filepath.Join("apps", encodePathSegment(app), "users", encodePathSegment(user), "sessions", encodePathSegment(session)+".jsonl")
}

func (p *paths) sessionDir(app, user string) string {
	return filepath.Join("apps", encodePathSegment(app), "users", encodePathSegment(user), "sessions")
}

func (p *paths) ensureParent(name string) error {
	parent := filepath.Dir(name)
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
	encoded := encodePathSegment(value)
	if len(encoded) > maxSegmentLen {
		return "", ErrInvalidID
	}
	return encoded, nil
}

func encodePathSegment(value string) string {
	encoded := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		b := value[i]
		if isUnreserved(b) && !(len(value) == 1 && b == '.') && !(len(value) == 2 && value == "..") {
			encoded = append(encoded, b)
			continue
		}
		encoded = append(encoded, '%', hexUpper(b>>4), hexUpper(b&0x0f))
	}
	return string(encoded)
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
