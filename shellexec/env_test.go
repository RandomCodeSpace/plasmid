package shellexec

import (
	"os"
	"testing"
)

func TestBuildEnvPrecedenceAndReplacement(t *testing.T) {
	t.Setenv("PLASMID_INHERITED", "yes")
	tests := []struct {
		name       string
		configured []string
		extra      map[string]string
		want       map[string]string
		absent     []string
	}{
		{
			name:       "inherits by default",
			configured: nil,
			want:       map[string]string{"PLASMID_INHERITED": "yes", "TERM": "dumb"},
		},
		{
			name:       "non-nil environment replaces inheritance and last duplicate wins",
			configured: []string{"BASE=first", "BASE=last", "TERM=host"},
			want:       map[string]string{"BASE": "last", "TERM": "dumb"},
			absent:     []string{"PLASMID_INHERITED", "CI"},
		},
		{
			name:       "extra environment overrides forced values",
			configured: []string{},
			extra:      map[string]string{"TERM": "extra", "PWD": "/reported", "CUSTOM": "value"},
			want:       map[string]string{"TERM": "extra", "PWD": "/reported", "CUSTOM": "value"},
			absent:     []string{"PLASMID_INHERITED", "CI"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := envMap(buildEnv(test.configured, test.extra, "/workspace"))
			want := forcedEnvironment("/workspace")
			for key, value := range test.extra {
				want[key] = value
			}
			for key, value := range test.want {
				want[key] = value
			}
			assertEnvironmentContains(t, got, want)
			assertEnvironmentAbsent(t, got, test.absent)
		})
	}
}

func forcedEnvironment(dir string) map[string]string {
	return map[string]string{
		"PWD":                 dir,
		"TERM":                "dumb",
		"PAGER":               "cat",
		"GIT_PAGER":           "cat",
		"GIT_TERMINAL_PROMPT": "0",
		"DEBIAN_FRONTEND":     "noninteractive",
		"NO_COLOR":            "1",
	}
}

func assertEnvironmentContains(t *testing.T, got, want map[string]string) {
	t.Helper()
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
}

func assertEnvironmentAbsent(t *testing.T, environment map[string]string, keys []string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := environment[key]; ok {
			t.Errorf("%s unexpectedly present", key)
		}
	}
}

func TestSplitEnv(t *testing.T) {
	tests := []struct {
		input, key, value string
	}{
		{"A=b=c", "A", "b=c"},
		{"EMPTY", "EMPTY", ""},
		{"=C:=C:\\work", "=C:", "C:\\work"},
	}
	for _, test := range tests {
		key, value := splitEnv(test.input)
		if key != test.key || value != test.value {
			t.Errorf("splitEnv(%q) = %q, %q; want %q, %q", test.input, key, value, test.key, test.value)
		}
	}
}

func envMap(env []string) map[string]string {
	result := make(map[string]string, len(env))
	for _, entry := range env {
		key, value := splitEnv(entry)
		result[key] = value
	}
	return result
}

func TestBuildEnvDoesNotMutateInputs(t *testing.T) {
	configured := []string{"A=one"}
	extra := map[string]string{"B": "two"}
	_ = buildEnv(configured, extra, os.TempDir())
	if configured[0] != "A=one" || extra["B"] != "two" {
		t.Fatal("buildEnv mutated its inputs")
	}
}
