package codingtools

import (
	"reflect"
	"testing"
)

func TestNormativeWireStructTags(t *testing.T) {
	tests := []struct {
		value any
		keys  []string
	}{
		{ReadArgs{}, []string{"path", "offset", "limit"}},
		{ReadResult{}, []string{"path", "content", "start_line", "end_line", "total_lines", "truncated", "report"}},
		{WriteArgs{}, []string{"path", "content"}},
		{WriteResult{}, []string{"path", "bytes_written", "diff", "truncated", "report"}},
		{EditArgs{}, []string{"path", "old_text", "new_text", "replace_all"}},
		{EditResult{}, []string{"path", "replacements", "match_tier", "diff", "truncated", "report"}},
		{BashArgs{}, []string{"command", "dir", "timeout_ms"}},
		{BashResult{}, []string{"stdout", "stderr", "exit_code", "signal", "timed_out", "killed", "truncated", "stdout_report", "stderr_report"}},
		{GrepArgs{}, []string{"pattern", "path", "glob", "literal", "case_insensitive", "context_lines", "max_results"}},
		{GrepResult{}, []string{"matches", "match_count", "files", "truncated", "skipped_binary", "skipped_too_large", "skipped_long_lines"}},
		{FindArgs{}, []string{"path", "glob", "type", "sort_by", "max_results"}},
		{FindResult{}, []string{"paths", "truncated"}},
		{ListArgs{}, []string{"path", "max_depth", "show_hidden", "max_results"}},
		{ListResult{}, []string{"entries", "truncated"}},
	}
	if len(tests) != 14 {
		t.Fatalf("normative wire struct count = %d, want 14", len(tests))
	}
	for _, test := range tests {
		typeOf := reflect.TypeOf(test.value)
		t.Run(typeOf.Name(), func(t *testing.T) {
			if typeOf.NumField() != len(test.keys) {
				t.Fatalf("field count = %d, want %d", typeOf.NumField(), len(test.keys))
			}
			for index, key := range test.keys {
				if got := typeOf.Field(index).Tag.Get("json"); got != key {
					t.Errorf("field %s tag = %q, want %q", typeOf.Field(index).Name, got, key)
				}
			}
		})
	}
}

func TestNestedWireStructTags(t *testing.T) {
	tests := []struct {
		value any
		keys  []string
	}{
		{GrepMatch{}, []string{"path", "line", "text", "before", "after"}},
		{ListEntry{}, []string{"path", "type", "size", "mod_time"}},
	}
	for _, test := range tests {
		typeOf := reflect.TypeOf(test.value)
		for index, key := range test.keys {
			if got := typeOf.Field(index).Tag.Get("json"); got != key {
				t.Errorf("%s.%s tag = %q, want %q", typeOf.Name(), typeOf.Field(index).Name, got, key)
			}
		}
	}
}
