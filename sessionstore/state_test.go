package sessionstore

import "testing"

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
