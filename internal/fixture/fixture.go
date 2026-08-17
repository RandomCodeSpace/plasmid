package fixture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var registeredAreas = struct {
	sync.RWMutex
	areas map[string]struct{}
}{areas: make(map[string]struct{})}

type Case struct {
	ID  string
	Dir string
}

func Register(area string) {
	if area == "" || area == "." || strings.ContainsAny(area, `/\\`) {
		panic(fmt.Sprintf("invalid fixture area %q", area))
	}
	registeredAreas.Lock()
	registeredAreas.areas[area] = struct{}{}
	registeredAreas.Unlock()
}

func Walk(t *testing.T, area string, run func(*testing.T, Case)) {
	walk(t, area, nil, run)
}

// WalkKinds runs only cases whose case.json kind is selected.
func WalkKinds(t *testing.T, area string, kinds []string, run func(*testing.T, Case)) {
	t.Helper()
	if len(kinds) == 0 {
		t.Fatal("fixture kind filter is empty")
	}
	selected := make(map[string]struct{}, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			t.Fatal("fixture kind is empty")
		}
		if _, exists := selected[kind]; exists {
			t.Fatalf("duplicate fixture kind %q", kind)
		}
		selected[kind] = struct{}{}
	}
	walk(t, area, selected, run)
}

func walk(t *testing.T, area string, kinds map[string]struct{}, run func(*testing.T, Case)) {
	t.Helper()
	assertRegistered(t, area)
	root := fixtureRoot(t, area)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("fixture area %q contains non-case entry %q", area, entry.Name())
		}
		testCase := Case{ID: entry.Name(), Dir: filepath.Join(root, entry.Name())}
		if kinds != nil {
			var metadata struct {
				Kind string `json:"kind"`
			}
			testCase.Decode(t, "case.json", &metadata)
			if _, selected := kinds[metadata.Kind]; !selected {
				continue
			}
		}
		entry := entry
		t.Run(entry.Name(), func(t *testing.T) {
			run(t, testCase)
		})
	}
}

func AssertCoverage(t *testing.T, area string) {
	t.Helper()
	assertRegistered(t, area)
	root := fixtureRoot(t, area)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatalf("fixture area %q has no cases", area)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("fixture area %q contains non-case entry %q", area, entry.Name())
		}
		dir := filepath.Join(root, entry.Name())
		for _, name := range []string{"case.json", "expected.json"} {
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil || !info.Mode().IsRegular() {
				t.Errorf("fixture %s/%s lacks regular %s", area, entry.Name(), name)
			}
		}
		_, inputFileErr := os.Stat(filepath.Join(dir, "input.json"))
		inputDirInfo, inputDirErr := os.Stat(filepath.Join(dir, "input"))
		hasInputFile := inputFileErr == nil
		hasInputDir := inputDirErr == nil && inputDirInfo.IsDir()
		if hasInputFile == hasInputDir {
			t.Errorf("fixture %s/%s must contain exactly one of input.json or input/", area, entry.Name())
		}
	}
}

func assertRegistered(t *testing.T, area string) {
	t.Helper()
	registeredAreas.RLock()
	_, ok := registeredAreas.areas[area]
	registeredAreas.RUnlock()
	if !ok {
		t.Fatalf("fixture area %q has no registered runner", area)
	}
}

func fixtureRoot(t *testing.T, area string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate fixture package")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..", "testdata", "fixtures", area)
}

func (c Case) Decode(t *testing.T, name string, destination any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.Dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	if err := validateSortedJSON(data); err != nil {
		t.Fatalf("validate %s: %v", name, err)
	}
}

func validateSortedJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON token")
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		previous := ""
		hasPrevious := false
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if hasPrevious && key <= previous {
				return fmt.Errorf("object key %q follows %q out of order", key, previous)
			}
			previous = key
			hasPrevious = true
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	_, err = decoder.Token()
	return err
}
