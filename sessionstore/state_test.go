package sessionstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitAndMergeState(t *testing.T) {
	session, app, user := splitState(map[string]any{"local": 1, "app:shared": 2, "user:shared": 3, "temp:discard": 4})
	if len(session) != 1 || session["local"] != 1 || app["shared"] != 2 || user["shared"] != 3 {
		t.Fatalf("split = %#v, %#v, %#v", session, app, user)
	}
	merged := mergeState(session, app, user)
	if len(merged) != 3 || merged["local"] != 1 || merged["app:shared"] != 2 || merged["user:shared"] != 3 {
		t.Fatalf("merge = %#v", merged)
	}
}

func TestStateSnapshotRoundTrip(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeStateSnapshot(root, "state.json", map[string]any{"saved": "state"}, true); err != nil {
		t.Fatal(err)
	}
	info, err := root.Stat("state.json")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	state, err := readStateSnapshot(root, "state.json")
	if err != nil {
		t.Fatal(err)
	}
	if state["saved"] != "state" {
		t.Fatalf("state = %#v", state)
	}
}

func TestReadMissingStateSnapshot(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	state, err := readStateSnapshot(root, "missing.json")
	if err != nil || len(state) != 0 {
		t.Fatalf("state, err = %#v, %v", state, err)
	}
}

func TestStateSnapshotRejectsNonCanonicalNames(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	for _, name := range []string{"", ".", "../state.json", filepath.Join(t.TempDir(), "state.json"), "dir/../state.json"} {
		if _, err := readStateSnapshot(root, name); err == nil {
			t.Errorf("readStateSnapshot(%q) succeeded", name)
		}
		if err := writeStateSnapshot(root, name, map[string]any{}, false); err == nil {
			t.Errorf("writeStateSnapshot(%q) succeeded", name)
		}
	}
}
