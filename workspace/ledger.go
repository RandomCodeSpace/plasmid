package workspace

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

var (
	ErrNeverRead = errors.New("workspace file was never read")
	ErrStaleRead = errors.New("workspace file changed since it was read")
)

type ledgerKey struct {
	sessionID string
	path      string
}

type ledgerEntry struct {
	hash [sha256.Size]byte
	size int64
	at   time.Time
}

// Ledger records the last full-content read or write for each session and path.
type Ledger struct {
	mu      sync.RWMutex
	entries map[ledgerKey]ledgerEntry
}

// NewLedger creates an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{entries: make(map[ledgerKey]ledgerEntry)}
}

// RecordRead records the full-content state observed by a successful read.
func (l *Ledger) RecordRead(sessionID, path string, size int64, hash [sha256.Size]byte) {
	l.record(sessionID, path, size, hash)
}

// RecordWrite records the full-content state produced by a successful write.
func (l *Ledger) RecordWrite(sessionID, path string, size int64, hash [sha256.Size]byte) {
	l.record(sessionID, path, size, hash)
}

func (l *Ledger) record(sessionID, path string, size int64, hash [sha256.Size]byte) {
	l.mu.Lock()
	l.entries[ledgerKey{sessionID: sessionID, path: normalizeLedgerPath(path)}] = ledgerEntry{hash: hash, size: size, at: time.Now()}
	l.mu.Unlock()
}

// Verify requires that the existing file matches the last state read by sessionID.
func (l *Ledger) Verify(sessionID, path string, size int64, hash [sha256.Size]byte) error {
	return l.VerifyWrite(sessionID, path, size, hash, true)
}

// VerifyWrite verifies an existing target. A nonexistent target may be created
// without a prior read by passing exists as false.
func (l *Ledger) VerifyWrite(sessionID, path string, size int64, hash [sha256.Size]byte, exists bool) error {
	if !exists {
		return nil
	}
	l.mu.RLock()
	entry, ok := l.entries[ledgerKey{sessionID: sessionID, path: normalizeLedgerPath(path)}]
	l.mu.RUnlock()
	if !ok {
		return fmt.Errorf("verify %q: %w", path, ErrNeverRead)
	}
	if entry.size != size || entry.hash != hash {
		return fmt.Errorf("verify %q: %w", path, ErrStaleRead)
	}
	return nil
}

// Forget removes the state for one session and path.
func (l *Ledger) Forget(sessionID, path string) {
	l.mu.Lock()
	delete(l.entries, ledgerKey{sessionID: sessionID, path: normalizeLedgerPath(path)})
	l.mu.Unlock()
}

func normalizeLedgerPath(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}

// ForgetSession removes all states associated with a session.
func (l *Ledger) ForgetSession(sessionID string) {
	l.mu.Lock()
	for key := range l.entries {
		if key.sessionID == sessionID {
			delete(l.entries, key)
		}
	}
	l.mu.Unlock()
}
