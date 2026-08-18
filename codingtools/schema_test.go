package codingtools

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type schemaAccessor func() json.RawMessage

type schemaExpectation struct {
	name        string
	accessor    schemaAccessor
	required    []string
	defaults    map[string]any
	minimums    map[string]float64
	enums       map[string][]any
	description string
}

func schemaExpectations() []schemaExpectation {
	return []schemaExpectation{
		{
			name: "read", accessor: ReadInputSchema, required: []string{"path"},
			defaults: map[string]any{"limit": float64(2000), "offset": float64(1)},
			minimums: map[string]float64{"limit": 1, "offset": 1}, description: ReadDescription,
		},
		{
			name: "write", accessor: WriteInputSchema, required: []string{"path", "content"},
			description: WriteDescription,
		},
		{
			name: "edit", accessor: EditInputSchema, required: []string{"path", "old_text", "new_text"},
			defaults: map[string]any{"replace_all": false}, description: EditDescription,
		},
		{
			name: "bash", accessor: BashInputSchema, required: []string{"command"},
			defaults: map[string]any{"dir": ".", "timeout_ms": float64(120000)},
			minimums: map[string]float64{"timeout_ms": 1}, description: BashDescription,
		},
		{
			name: "grep", accessor: GrepInputSchema, required: []string{"pattern"},
			defaults: map[string]any{
				"case_insensitive": false, "context_lines": float64(0), "literal": false,
				"max_results": float64(200), "path": ".",
			},
			minimums: map[string]float64{"context_lines": 0, "max_results": 1}, description: GrepDescription,
		},
		{
			name: "find", accessor: FindInputSchema, required: []string{"glob"},
			defaults: map[string]any{
				"max_results": float64(200), "path": ".", "sort_by": "path", "type": "any",
			},
			minimums: map[string]float64{"max_results": 1},
			enums: map[string][]any{
				"sort_by": {"path", "modified"}, "type": {"file", "dir", "symlink", "any"},
			},
			description: FindDescription,
		},
		{
			name: "ls", accessor: ListInputSchema, required: []string{},
			defaults: map[string]any{
				"max_depth": float64(1), "max_results": float64(20000), "path": ".", "show_hidden": false,
			},
			minimums: map[string]float64{"max_depth": 1, "max_results": 1}, description: ListDescription,
		},
	}
}

func TestInputSchemas(t *testing.T) {
	for _, test := range schemaExpectations() {
		t.Run(test.name, func(t *testing.T) {
			assertInputSchema(t, test)
		})
	}
}

func assertInputSchema(t *testing.T, test schemaExpectation) {
	t.Helper()
	raw := test.accessor()
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	canonical, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, canonical) {
		t.Fatalf("schema is not canonical JSON\ngot:  %s\nwant: %s", raw, canonical)
	}
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema envelope = %#v, want object with additionalProperties false", schema)
	}
	properties := schemaProperties(t, schema)
	assertPropertyDescriptions(t, properties)
	assertRequired(t, schema, test.required)
	assertSchemaValues(t, properties, "default", test.defaults)
	assertSchemaValues(t, properties, "minimum", floatMapToAny(test.minimums))
	assertSchemaValues(t, properties, "enum", sliceMapToAny(test.enums))
	assertNoReservedProperties(t, schema, "$")
}

func schemaProperties(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %#v, want object", schema["properties"])
	}
	return properties
}

func assertPropertyDescriptions(t *testing.T, properties map[string]any) {
	t.Helper()
	for property, value := range properties {
		definition, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("property %q = %#v, want object", property, value)
		}
		if description, ok := definition["description"].(string); !ok || description == "" {
			t.Errorf("property %q lacks a description", property)
		}
	}
}

func TestInputSchemaDefensiveCopies(t *testing.T) {
	for _, test := range schemaExpectations() {
		t.Run(test.name, func(t *testing.T) {
			first := test.accessor()
			second := test.accessor()
			if len(first) == 0 || len(second) == 0 {
				t.Fatal("empty schema")
			}
			first[0] ^= 0xff
			third := test.accessor()
			if !bytes.Equal(second, third) {
				t.Fatal("caller mutation changed canonical schema bytes")
			}
		})
	}
}

func TestDescriptions(t *testing.T) {
	const sandboxSentence = "paths are relative to the working directory; paths outside it are rejected"
	for _, test := range schemaExpectations() {
		t.Run(test.name, func(t *testing.T) {
			assertDescription(t, test, sandboxSentence)
		})
	}
	if !strings.Contains(BashDescription, "host-process authority") || !strings.Contains(BashDescription, "not confined") {
		t.Fatal("bash description does not state host-process authority and lack of file-tool confinement")
	}
	if !strings.Contains(BashDescription, "initial working directory") || !strings.Contains(BashDescription, "workspace root") {
		t.Fatal("bash description does not state dir behavior and its default")
	}
	if !strings.Contains(WriteDescription, "prior successful read") {
		t.Fatal("write description does not state the overwrite prior-read precondition")
	}
	if !strings.Contains(EditDescription, "prior successful read") {
		t.Fatal("edit description does not state the prior-read precondition")
	}
}

func assertDescription(t *testing.T, test schemaExpectation, sandboxSentence string) {
	t.Helper()
	if words := len(strings.Fields(test.description)); words > 120 {
		t.Fatalf("description has %d words, want at most 120", words)
	}
	if !strings.Contains(strings.ToLower(test.description), "truncat") {
		t.Fatal("description does not state truncation behavior")
	}
	if test.name != "bash" && !strings.Contains(test.description, sandboxSentence) {
		t.Fatalf("description lacks exact sandbox sentence %q", sandboxSentence)
	}
}

func TestResultsMarshalToUnreservedObjects(t *testing.T) {
	results := []struct {
		name  string
		value any
	}{
		{"read", ReadResult{}},
		{"write", WriteResult{}},
		{"edit", EditResult{}},
		{"bash", BashResult{}},
		{"grep", GrepResult{}},
		{"find", FindResult{}},
		{"ls", ListResult{}},
	}
	for _, test := range results {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := json.Unmarshal(encoded, &object); err != nil {
				t.Fatalf("result is not a JSON object: %v", err)
			}
			for _, reserved := range []string{"diagnostics", "diagnostics_text"} {
				if _, exists := object[reserved]; exists {
					t.Fatalf("result defines reserved key %q", reserved)
				}
			}
		})
	}
}

func TestResultFieldsArePortable(t *testing.T) {
	resultTypes := []reflect.Type{
		reflect.TypeOf(ReadResult{}), reflect.TypeOf(WriteResult{}), reflect.TypeOf(EditResult{}),
		reflect.TypeOf(BashResult{}), reflect.TypeOf(GrepResult{}), reflect.TypeOf(FindResult{}),
		reflect.TypeOf(ListResult{}),
	}
	banned := map[reflect.Type]string{
		reflect.TypeOf(time.Duration(0)): "time.Duration",
		reflect.TypeOf(fs.FileMode(0)):   "fs.FileMode",
	}
	for _, resultType := range resultTypes {
		assertPortableType(t, resultType.Name(), resultType, banned)
	}
}

func assertPortableType(t *testing.T, path string, value reflect.Type, banned map[reflect.Type]string) {
	t.Helper()
	if name, exists := banned[value]; exists {
		t.Errorf("%s exposes non-portable %s", path, name)
		return
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		t.Errorf("%s exposes non-portable %s", path, value)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			assertPortableType(t, path+"."+field.Name, field.Type, banned)
		}
	case reflect.Array, reflect.Pointer, reflect.Slice:
		assertPortableType(t, path+"[]", value.Elem(), banned)
	case reflect.Map:
		assertPortableType(t, path+"{key}", value.Key(), banned)
		assertPortableType(t, path+"{value}", value.Elem(), banned)
	}
}

func assertRequired(t *testing.T, schema map[string]any, want []string) {
	t.Helper()
	raw, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("required = %#v, want array", schema["required"])
	}
	got := make([]string, len(raw))
	for index, value := range raw {
		var stringOK bool
		got[index], stringOK = value.(string)
		if !stringOK {
			t.Fatalf("required[%d] = %#v, want string", index, value)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required = %#v, want %#v", got, want)
	}
}

func assertSchemaValues(t *testing.T, properties map[string]any, keyword string, want map[string]any) {
	t.Helper()
	for property, expected := range want {
		definition, ok := properties[property].(map[string]any)
		if !ok {
			t.Fatalf("property %q is missing", property)
		}
		if !reflect.DeepEqual(definition[keyword], expected) {
			t.Errorf("%s.%s = %#v, want %#v", property, keyword, definition[keyword], expected)
		}
	}
}

func assertNoReservedProperties(t *testing.T, value any, path string) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		assertNoReservedMapProperties(t, typed, path)
	case []any:
		for _, item := range typed {
			assertNoReservedProperties(t, item, path+"[]")
		}
	}
}

func assertNoReservedMapProperties(t *testing.T, value map[string]any, path string) {
	t.Helper()
	if properties, ok := value["properties"].(map[string]any); ok {
		for _, reserved := range []string{"diagnostics", "diagnostics_text"} {
			if _, exists := properties[reserved]; exists {
				t.Errorf("%s.properties contains reserved property %q", path, reserved)
			}
		}
	}
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		assertNoReservedProperties(t, value[key], path+"."+key)
	}
}

func floatMapToAny(values map[string]float64) map[string]any {
	converted := make(map[string]any, len(values))
	for key, value := range values {
		converted[key] = value
	}
	return converted
}

func sliceMapToAny(values map[string][]any) map[string]any {
	converted := make(map[string]any, len(values))
	for key, value := range values {
		converted[key] = value
	}
	return converted
}
