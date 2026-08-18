package shellexec

import (
	"os"
	"sort"
	"strings"
)

func buildEnv(configured []string, extra map[string]string, dir string) []string {
	base := configured
	if configured == nil {
		base = os.Environ()
	}

	order := make([]string, 0, len(base)+7+len(extra))
	values := make(map[string]string, len(base)+7+len(extra))
	set := func(key, value string) {
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	for _, entry := range base {
		key, value := splitEnv(entry)
		set(key, value)
	}
	for _, entry := range []struct{ key, value string }{
		{"PWD", dir},
		{"TERM", "dumb"},
		{"PAGER", "cat"},
		{"GIT_PAGER", "cat"},
		{"GIT_TERMINAL_PROMPT", "0"},
		{"DEBIAN_FRONTEND", "noninteractive"},
		{"NO_COLOR", "1"},
	} {
		set(entry.key, entry.value)
	}
	extraKeys := make([]string, 0, len(extra))
	for key := range extra {
		extraKeys = append(extraKeys, key)
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		set(key, extra[key])
	}

	result := make([]string, 0, len(order))
	for _, key := range order {
		result = append(result, key+"="+values[key])
	}
	return result
}

func splitEnv(entry string) (string, string) {
	start := 0
	if strings.HasPrefix(entry, "=") {
		start = 1
	}
	index := strings.IndexByte(entry[start:], '=')
	if index < 0 {
		return entry, ""
	}
	index += start
	return entry[:index], entry[index+1:]
}
