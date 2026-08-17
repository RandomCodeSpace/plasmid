package workspace

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestLedgerVerification(t *testing.T) {
	ledger := NewLedger()
	old := contentHash("old")
	newer := contentHash("new")
	ledger.RecordRead("one", "file.txt", 3, old)

	tests := []struct {
		name    string
		session string
		path    string
		size    int64
		hash    [sha256.Size]byte
		exists  bool
		want    error
	}{
		{name: "unchanged", session: "one", path: "file.txt", size: 3, hash: old, exists: true},
		{name: "never read", session: "one", path: "other.txt", size: 3, hash: old, exists: true, want: ErrNeverRead},
		{name: "stale", session: "one", path: "file.txt", size: 3, hash: newer, exists: true, want: ErrStaleRead},
		{name: "new file", session: "one", path: "new.txt", exists: false},
		{name: "session isolated", session: "two", path: "file.txt", size: 3, hash: old, exists: true, want: ErrNeverRead},
		{name: "normalized path", session: "one", path: "dir/../file.txt", size: 3, hash: old, exists: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ledger.VerifyWrite(test.session, test.path, test.size, test.hash, test.exists)
			if !errors.Is(err, test.want) {
				t.Fatalf("VerifyWrite error = %v, want %v", err, test.want)
			}
		})
	}

	ledger.RecordWrite("one", "file.txt", 3, newer)
	if err := ledger.Verify("one", "file.txt", 3, newer); err != nil {
		t.Fatalf("post-write Verify error = %v", err)
	}
	ledger.Forget("one", "file.txt")
	if err := ledger.Verify("one", "file.txt", 3, newer); !errors.Is(err, ErrNeverRead) {
		t.Fatalf("Forget Verify error = %v", err)
	}
}

func TestLedgerNormalizesPathsForWriteAndForget(t *testing.T) {
	ledger := NewLedger()
	hash := contentHash("content")
	ledger.RecordWrite("session", "dir/../file.txt", 7, hash)
	if err := ledger.Verify("session", "file.txt", 7, hash); err != nil {
		t.Fatalf("Verify normalized path: %v", err)
	}
	ledger.Forget("session", "dir/../file.txt")
	if err := ledger.Verify("session", "file.txt", 7, hash); !errors.Is(err, ErrNeverRead) {
		t.Fatalf("Verify after Forget: %v", err)
	}
}

func TestLedgerConcurrentIsolation(t *testing.T) {
	tests := []struct {
		name    string
		session func(int) string
		path    func(int) string
	}{
		{name: "same path across sessions", session: func(index int) string { return fmt.Sprintf("session-%d", index) }, path: func(int) string { return "shared.txt" }},
		{name: "same session across paths", session: func(int) string { return "shared-session" }, path: func(index int) string { return fmt.Sprintf("path-%d", index) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ledger := NewLedger()
			const records = 64
			type record struct {
				session string
				path    string
				hash    [sha256.Size]byte
			}
			entries := make([]record, records)
			var group sync.WaitGroup
			for index := range records {
				session := test.session(index)
				path := test.path(index)
				entries[index] = record{
					session: session,
					path:    path,
					hash:    contentHash(fmt.Sprintf("%s/%s", session, path)),
				}
				group.Add(1)
				go func(index int) {
					defer group.Done()
					entry := entries[index]
					ledger.RecordRead(entry.session, entry.path, int64(len(entry.path)), entry.hash)
				}(index)
			}
			group.Wait()
			for _, entry := range entries {
				if err := ledger.Verify(entry.session, entry.path, int64(len(entry.path)), entry.hash); err != nil {
					t.Errorf("Verify(%s, %s): %v", entry.session, entry.path, err)
				}
			}
		})
	}
}

func contentHash(content string) [sha256.Size]byte { return sha256.Sum256([]byte(content)) }
