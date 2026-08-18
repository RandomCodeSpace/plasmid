package fixture

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/plasmid-dev/plasmid/warning"
)

type runnerKey struct {
	area string
	name string
}

type runnerRegistration struct {
	executions []caseExecution
	kinds      map[string]struct{}
}

type caseExecution struct {
	id      string
	kind    string
	receipt *comparisonReceipt
}

var registeredRunners = struct {
	sync.RWMutex
	runners map[runnerKey]*runnerRegistration
}{runners: make(map[runnerKey]*runnerRegistration)}

// GoldenMode makes golden-file mutation an explicit call-site decision.
type GoldenMode uint8

const (
	GoldenReadOnly GoldenMode = iota
	GoldenUpdate
)

// Paths contains the only values that portable fixtures may substitute.
// Values are normalized to slash-separated paths before use.
type Paths struct {
	Home      string
	WorkDir   string
	ConfigDir string
}

// Metadata is the common portion of every case.json file.
type Metadata struct {
	Area string `json:"area"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// WarningFields is the stable fixture projection of a warning. Human-readable
// messages are deliberately excluded from golden compatibility.
type WarningFields struct {
	Code   string `json:"code"`
	Source string `json:"source"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
}

type Case struct {
	Area    string
	ID      string
	Dir     string
	receipt *comparisonReceipt
}

type comparisonReceipt struct {
	expectedCompared atomic.Bool
}

// RegisterRunner declares the exact fixture kinds executed by a named test
// runner. A zero-kind registration is dormant and owns no fixture cases.
func RegisterRunner(area, runner string, kinds ...string) {
	if err := validateName("area", area); err != nil {
		panic(err)
	}
	if err := validateRunnerName(runner); err != nil {
		panic(err)
	}
	owned := make(map[string]struct{}, len(kinds))
	for _, kind := range kinds {
		if err := validateKind(kind); err != nil {
			panic(err)
		}
		if _, duplicate := owned[kind]; duplicate {
			panic(fmt.Sprintf("fixture runner %q registers duplicate kind %q", runner, kind))
		}
		owned[kind] = struct{}{}
	}
	key := runnerKey{area: area, name: runner}
	registeredRunners.Lock()
	defer registeredRunners.Unlock()
	if _, duplicate := registeredRunners.runners[key]; duplicate {
		panic(fmt.Sprintf("duplicate fixture runner %q for area %q", runner, area))
	}
	registeredRunners.runners[key] = &runnerRegistration{kinds: owned}
}

// Run executes a package test binary and verifies its registered fixture
// runners before returning the process exit code.
func Run(m *testing.M) int {
	code := m.Run()
	root, err := fixtureBaseRoot()
	var problems []error
	if err != nil {
		problems = append(problems, err)
	} else {
		problems = verifyRunnerReceipts(
			filepath.Join(root, "testdata", "fixtures"), snapshotRunners(),
			requireAllRunnerReceipts(testFlagValue("test.run"), testFlagValue("test.list")),
		)
	}
	for _, problem := range problems {
		fmt.Fprintf(os.Stderr, "fixture: %v\n", problem)
	}
	if len(problems) != 0 && code == 0 {
		return 1
	}
	return code
}

func testFlagValue(name string) string {
	option := flag.Lookup(name)
	if option == nil {
		return ""
	}
	return option.Value.String()
}

func requireAllRunnerReceipts(runFilter, listFilter string) bool {
	if listFilter != "" {
		return false
	}
	switch runFilter {
	case "", ".", ".*", "^.*$":
		return true
	default:
		return false
	}
}

type runnerSnapshot struct {
	executions []caseExecution
	key        runnerKey
	kinds      []string
}

func snapshotRunners() []runnerSnapshot {
	registeredRunners.RLock()
	defer registeredRunners.RUnlock()
	snapshots := make([]runnerSnapshot, 0, len(registeredRunners.runners))
	for key, registration := range registeredRunners.runners {
		kinds := make([]string, 0, len(registration.kinds))
		for kind := range registration.kinds {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		snapshots = append(snapshots, runnerSnapshot{
			executions: append([]caseExecution(nil), registration.executions...), key: key, kinds: kinds,
		})
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].key.area != snapshots[j].key.area {
			return snapshots[i].key.area < snapshots[j].key.area
		}
		return snapshots[i].key.name < snapshots[j].key.name
	})
	return snapshots
}

func verifyRunnerReceipts(fixturesRoot string, snapshots []runnerSnapshot, requireAll bool) []error {
	areaCases := make(map[string]map[string][]string)
	var problems []error
	for _, snapshot := range snapshots {
		if len(snapshot.kinds) == 0 {
			continue
		}
		cases, ok := areaCases[snapshot.key.area]
		if !ok {
			var errors []error
			cases, errors = loadAreaCaseKinds(fixturesRoot, snapshot.key.area)
			problems = append(problems, errors...)
			areaCases[snapshot.key.area] = cases
		}
		executed := make(map[string]map[string]bool)
		for _, execution := range snapshot.executions {
			if executed[execution.kind] == nil {
				executed[execution.kind] = make(map[string]bool)
			}
			executed[execution.kind][execution.id] = true
			if execution.receipt == nil || !execution.receipt.expectedCompared.Load() {
				problems = append(problems, fmt.Errorf("runner %q case %s/%s executed without CompareJSON for expected.json", snapshot.key.name, snapshot.key.area, execution.id))
			}
		}
		for _, kind := range snapshot.kinds {
			ids := cases[kind]
			if len(ids) == 0 {
				problems = append(problems, fmt.Errorf("runner %q registers absent kind %q in area %q", snapshot.key.name, kind, snapshot.key.area))
				continue
			}
			if !requireAll {
				continue
			}
			for _, id := range ids {
				if !executed[kind][id] {
					problems = append(problems, fmt.Errorf("runner %q did not execute fixture case %s/%s of kind %q", snapshot.key.name, snapshot.key.area, id, kind))
				}
			}
		}
	}
	sort.Slice(problems, func(i, j int) bool { return problems[i].Error() < problems[j].Error() })
	return problems
}

func loadAreaCaseKinds(fixturesRoot, area string) (map[string][]string, []error) {
	result := make(map[string][]string)
	areaRoot := filepath.Join(fixturesRoot, area)
	entries, err := os.ReadDir(areaRoot)
	if err != nil {
		return result, []error{fmt.Errorf("read fixture area %q: %w", area, err)}
	}
	var problems []error
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			problems = append(problems, fmt.Errorf("fixture area %q contains non-case entry %q", area, entry.Name()))
			continue
		}
		path := filepath.Join(areaRoot, entry.Name(), "case.json")
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			problems = append(problems, fmt.Errorf("read %s: %w", path, readErr))
			continue
		}
		var metadata Metadata
		if decodeErr := json.Unmarshal(data, &metadata); decodeErr != nil {
			problems = append(problems, fmt.Errorf("decode %s: %w", path, decodeErr))
			continue
		}
		if metadata.Area != area || metadata.ID != entry.Name() || metadata.Kind == "" {
			problems = append(problems, fmt.Errorf("fixture metadata mismatch in %s", path))
			continue
		}
		result[metadata.Kind] = append(result[metadata.Kind], metadata.ID)
	}
	for kind := range result {
		sort.Strings(result[kind])
	}
	return result, problems
}

func Walk(t *testing.T, area, runner string, run func(*testing.T, Case)) {
	_ = walk(t, area, runner, nil, run)
}

// WalkKinds runs only cases whose case.json kind is selected.
func WalkKinds(t *testing.T, area, runner string, kinds []string, run func(*testing.T, Case)) {
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
	matched := walk(t, area, runner, selected, run)
	for _, kind := range unmatchedKinds(kinds, matched) {
		t.Errorf("fixture area %q has no cases of kind %q", area, kind)
	}
}

func walk(t *testing.T, area, runner string, kinds map[string]struct{}, run func(*testing.T, Case)) map[string]int {
	t.Helper()
	owned := registeredKinds(t, area, runner)
	for kind := range kinds {
		if _, ok := owned[kind]; !ok {
			t.Fatalf("fixture runner %q does not register kind %q in area %q", runner, kind, area)
		}
	}
	matched := make(map[string]int, len(kinds))
	root := fixtureRoot(t, area)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireNonEmptyArea(area, len(entries)); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("fixture area %q contains non-case entry %q", area, entry.Name())
		}
		testCase := Case{Area: area, ID: entry.Name(), Dir: filepath.Join(root, entry.Name())}
		metadata := testCase.Metadata(t)
		if kinds != nil {
			if _, selected := kinds[metadata.Kind]; !selected {
				continue
			}
			matched[metadata.Kind]++
		} else if _, ok := owned[metadata.Kind]; !ok {
			t.Fatalf("fixture runner %q does not register case kind %q in area %q", runner, metadata.Kind, area)
		}
		t.Run(entry.Name(), func(t *testing.T) {
			receipt := &comparisonReceipt{}
			testCase.receipt = receipt
			recordCaseExecution(runnerKey{area: area, name: runner}, metadata.Kind, testCase.ID, receipt)
			defer func() {
				if err := testCase.comparisonError(); err != nil {
					t.Error(err)
				}
			}()
			run(t, testCase)
		})
	}
	return matched
}

func requireNonEmptyArea(area string, cases int) error {
	if cases == 0 {
		return fmt.Errorf("fixture area %q has no cases", area)
	}
	return nil
}

func unmatchedKinds(requested []string, matched map[string]int) []string {
	missing := make([]string, 0, len(requested))
	for _, kind := range requested {
		if matched[kind] == 0 {
			missing = append(missing, kind)
		}
	}
	return missing
}

// AssertCoverage validates the layout and common metadata of every case in an
// area. Subsystem tests remain responsible for interpreting kind-specific data.
func AssertCoverage(t *testing.T, area string) {
	t.Helper()
	assertAreaRegistered(t, area)
	for _, err := range validateArea(fixtureRoot(t, area), area) {
		t.Error(err)
	}
}

func assertAreaRegistered(t *testing.T, area string) {
	t.Helper()
	registeredRunners.RLock()
	found := false
	for key := range registeredRunners.runners {
		if key.area == area {
			found = true
			break
		}
	}
	registeredRunners.RUnlock()
	if !found {
		t.Fatalf("fixture area %q has no registered runner", area)
	}
}

func registeredKinds(t *testing.T, area, runner string) map[string]struct{} {
	t.Helper()
	registeredRunners.RLock()
	registration, ok := registeredRunners.runners[runnerKey{area: area, name: runner}]
	registeredRunners.RUnlock()
	if !ok {
		t.Fatalf("fixture runner %q is not registered for area %q", runner, area)
	}
	return registration.kinds
}

func recordCaseExecution(key runnerKey, kind, id string, receipt *comparisonReceipt) {
	registeredRunners.Lock()
	defer registeredRunners.Unlock()
	registration := registeredRunners.runners[key]
	if registration != nil {
		registration.executions = append(registration.executions, caseExecution{id: id, kind: kind, receipt: receipt})
	}
}

func fixtureRoot(t *testing.T, area string) string {
	t.Helper()
	root, err := fixtureBaseRoot()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "testdata", "fixtures", area)
}

func fixtureBaseRoot() (string, error) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("locate fixture package")
	}
	return filepath.Join(filepath.Dir(filename), "..", ".."), nil
}

// Metadata loads and validates the common fixture metadata.
func (c Case) Metadata(t *testing.T) Metadata {
	t.Helper()
	var metadata Metadata
	c.Decode(t, "case.json", &metadata)
	area := c.Area
	if area == "" {
		area = filepath.Base(filepath.Dir(c.Dir))
	}
	if metadata.Area != area || metadata.ID != c.ID || metadata.Kind == "" {
		t.Fatalf("fixture metadata = %#v, want area %q id %q and a non-empty kind", metadata, area, c.ID)
	}
	return metadata
}

// Read loads a fixture file and expands portable path placeholders once.
func (c Case) Read(t *testing.T, name string, paths Paths) []byte {
	t.Helper()
	data, err := c.read(name)
	if err != nil {
		t.Fatal(err)
	}
	return Expand(data, paths)
}

func (c Case) Decode(t *testing.T, name string, destination any) {
	t.Helper()
	if err := c.decode(name, destination, nil); err != nil {
		t.Fatal(err)
	}
}

// DecodePaths loads sorted JSON, expands placeholders in JSON string values,
// and decodes it into destination.
func (c Case) DecodePaths(t *testing.T, name string, destination any, paths Paths) {
	t.Helper()
	if err := c.decode(name, destination, &paths); err != nil {
		t.Fatal(err)
	}
}

func (c Case) decode(name string, destination any, paths *Paths) error {
	data, err := c.read(name)
	if err != nil {
		return err
	}
	if err := validateSortedJSON(data); err != nil {
		return fmt.Errorf("validate %s: %w", name, err)
	}
	if paths == nil {
		if err := json.Unmarshal(data, destination); err != nil {
			return fmt.Errorf("decode %s: %w", name, err)
		}
		return nil
	}
	value, err := decodeJSON(data)
	if err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	value, err = transformStrings(value, func(value string) (string, error) {
		return string(Expand([]byte(value), *paths)), nil
	})
	if err != nil {
		return fmt.Errorf("expand %s: %w", name, err)
	}
	data, err = json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s after substitution: %w", name, err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return nil
}

// CompareJSON structurally compares actual with a JSON golden. GoldenUpdate
// atomically replaces the golden with deterministic, placeholder-normalized
// JSON before performing the same read-only comparison.
func (c Case) CompareJSON(t *testing.T, name string, actual any, paths Paths, mode GoldenMode) {
	t.Helper()
	if c.receipt != nil && name == "expected.json" {
		c.receipt.expectedCompared.Store(true)
	}
	if err := compareJSON(c, name, actual, paths, mode); err != nil {
		t.Fatal(err)
	}
}

func (c Case) comparisonError() error {
	if c.receipt != nil && !c.receipt.expectedCompared.Load() {
		return fmt.Errorf("fixture %s/%s executed without CompareJSON for expected.json", c.Area, c.ID)
	}
	return nil
}

// StableWarnings removes warning prose while retaining stable structured
// fields in producer order. Paths use slash separators, and an empty input
// projects to [] rather than null.
func StableWarnings(values []warning.Warning) []WarningFields {
	projected := make([]WarningFields, len(values))
	for index, value := range values {
		projected[index] = WarningFields{
			Code: value.Code, Source: value.Source, Path: strings.ReplaceAll(filepath.ToSlash(value.Path), `\`, "/"), Line: value.Line,
		}
	}
	return projected
}

// Expand replaces ${HOME}, ${WORKDIR}, and ${CONFIG_DIR} in one non-recursive
// pass. Other placeholders are fixture content and remain unchanged.
func Expand(data []byte, paths Paths) []byte {
	values := substitutionValues(paths)
	var output strings.Builder
	input := string(data)
	for {
		start := strings.Index(input, "${")
		if start < 0 {
			output.WriteString(input)
			return []byte(output.String())
		}
		output.WriteString(input[:start])
		end := strings.IndexByte(input[start+2:], '}')
		if end < 0 {
			output.WriteString(input)
			return []byte(output.String())
		}
		end += start + 2
		placeholder := input[start : end+1]
		value, ok := values[placeholder]
		if !ok || value == "" {
			output.WriteString(placeholder)
			input = input[end+1:]
			continue
		}
		rest := input[end+1:]
		if strings.HasSuffix(output.String(), "file://") {
			value = fileURIReference(value)
		}
		output.WriteString(value)
		if strings.HasSuffix(value, "/") && strings.HasPrefix(rest, "/") {
			rest = rest[1:]
		}
		input = rest
	}
}

func (c Case) read(name string) ([]byte, error) {
	if _, err := casePath(c.Dir, name); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(c.Dir)
	if err != nil {
		return nil, fmt.Errorf("open fixture case: %w", err)
	}
	defer root.Close()
	name = filepath.Clean(name)
	if err := validateRootPath(root, name, false); err != nil {
		return nil, fmt.Errorf("confine fixture %s: %w", name, err)
	}
	data, err := root.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read fixture %s: %w", name, err)
	}
	return data, nil
}

func compareJSON(c Case, name string, actual any, paths Paths, mode GoldenMode) error {
	if mode != GoldenReadOnly && mode != GoldenUpdate {
		return fmt.Errorf("invalid golden mode %d", mode)
	}
	actualValue, err := jsonValue(actual)
	if err != nil {
		return fmt.Errorf("encode actual JSON: %w", err)
	}
	normalizedActual, err := normalizeJSONPaths(actualValue, paths)
	if err != nil {
		return fmt.Errorf("normalize actual JSON: %w", err)
	}
	portableActual, err := collapseJSONPaths(actualValue, paths)
	if err != nil {
		return fmt.Errorf("collapse actual JSON: %w", err)
	}
	if _, err := casePath(c.Dir, name); err != nil {
		return err
	}
	if mode == GoldenUpdate {
		data, marshalErr := json.MarshalIndent(portableActual, "", "  ")
		if marshalErr != nil {
			return fmt.Errorf("encode golden %s: %w", name, marshalErr)
		}
		data = append(data, '\n')
		if err := writeAtomic(c.Dir, name, data); err != nil {
			return fmt.Errorf("update golden %s: %w", name, err)
		}
	}
	expectedData, err := c.read(name)
	if err != nil {
		return fmt.Errorf("read golden %s: %w", name, err)
	}
	if err := validateSortedJSON(expectedData); err != nil {
		return fmt.Errorf("validate golden %s: %w", name, err)
	}
	expectedValue, err := decodeJSON(expectedData)
	if err != nil {
		return fmt.Errorf("decode golden %s: %w", name, err)
	}
	expectedValue, err = expandJSONPaths(expectedValue, paths)
	if err != nil {
		return fmt.Errorf("expand golden %s: %w", name, err)
	}
	if reflect.DeepEqual(normalizedActual, expectedValue) {
		return nil
	}
	got, _ := json.MarshalIndent(normalizedActual, "", "  ")
	want, _ := json.MarshalIndent(expectedValue, "", "  ")
	return fmt.Errorf("golden %s mismatch\ngot:  %s\nwant: %s", name, got, want)
}

func jsonValue(value any) (any, error) {
	var data []byte
	switch value := value.(type) {
	case json.RawMessage:
		data = value
	case []byte:
		data = value
	default:
		var err error
		data, err = json.Marshal(value)
		if err != nil {
			return nil, err
		}
	}
	return decodeJSON(data)
}

func decodeJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, errors.New("unexpected trailing JSON value")
		}
		return nil, err
	}
	return value, nil
}

func expandJSONPaths(value any, paths Paths) (any, error) {
	return transformStrings(value, func(value string) (string, error) {
		return string(Expand([]byte(value), paths)), nil
	})
}

func collapseJSONPaths(value any, paths Paths) (any, error) {
	replacements, err := pathReplacements(paths)
	if err != nil {
		return nil, err
	}
	return transformStrings(value, func(value string) (string, error) {
		value = collapseFileURIs(value, replacements)
		for _, replacement := range replacements {
			value = collapsePathSpans(value, replacement)
		}
		return value, nil
	})
}

func normalizeJSONPaths(value any, paths Paths) (any, error) {
	replacements, err := pathReplacements(paths)
	if err != nil {
		return nil, err
	}
	return transformStrings(value, func(value string) (string, error) {
		value = normalizeFileURIs(value, replacements)
		for _, replacement := range replacements {
			replacement.placeholder = replacement.portable
			value = collapsePathSpans(value, replacement)
		}
		return value, nil
	})
}

func transformStrings(value any, transform func(string) (string, error)) (any, error) {
	switch value := value.(type) {
	case string:
		return transform(value)
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			transformed, err := transformStrings(item, transform)
			if err != nil {
				return nil, err
			}
			result[index] = transformed
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			transformed, err := transformStrings(item, transform)
			if err != nil {
				return nil, err
			}
			result[key] = transformed
		}
		return result, nil
	default:
		return value, nil
	}
}

type pathReplacement struct {
	placeholder string
	native      string
	portable    string
}

func pathReplacements(paths Paths) ([]pathReplacement, error) {
	values := []pathReplacement{
		{placeholder: "${CONFIG_DIR}", native: canonicalNativeFixturePath(paths.ConfigDir), portable: canonicalFixturePath(paths.ConfigDir)},
		{placeholder: "${HOME}", native: canonicalNativeFixturePath(paths.Home), portable: canonicalFixturePath(paths.Home)},
		{placeholder: "${WORKDIR}", native: canonicalNativeFixturePath(paths.WorkDir), portable: canonicalFixturePath(paths.WorkDir)},
	}
	replacements := make([]pathReplacement, 0, len(values))
	seen := make(map[string]string)
	for _, replacement := range values {
		if replacement.portable == "" {
			continue
		}
		if existing, ok := seen[replacement.portable]; ok && existing != replacement.placeholder {
			return nil, fmt.Errorf("fixture substitution value is shared by %s and %s", existing, replacement.placeholder)
		}
		seen[replacement.portable] = replacement.placeholder
		replacements = append(replacements, replacement)
	}
	sort.Slice(replacements, func(i, j int) bool {
		if len(replacements[i].portable) != len(replacements[j].portable) {
			return len(replacements[i].portable) > len(replacements[j].portable)
		}
		return replacements[i].placeholder < replacements[j].placeholder
	})
	return replacements, nil
}

func substitutionValues(paths Paths) map[string]string {
	return map[string]string{
		"${CONFIG_DIR}": canonicalFixturePath(paths.ConfigDir),
		"${HOME}":       canonicalFixturePath(paths.Home),
		"${WORKDIR}":    canonicalFixturePath(paths.WorkDir),
	}
}

func portablePath(value string) string {
	return strings.ReplaceAll(filepath.ToSlash(value), `\`, "/")
}

func canonicalFixturePath(value string) string {
	value = portablePath(value)
	if strings.HasPrefix(value, "//") {
		withoutPrefix := strings.TrimPrefix(value, "//")
		host, rest, found := strings.Cut(withoutPrefix, "/")
		if !found {
			value = "//" + strings.ToLower(host)
		} else {
			value = "//" + strings.ToLower(host) + "/" + rest
		}
	}
	return trimTrailingPathSeparators(value)
}

func canonicalNativeFixturePath(value string) string {
	for len(value) > 1 && (strings.HasSuffix(value, "/") || strings.HasSuffix(value, `\`)) {
		if isDriveRoot(portablePath(value)) {
			break
		}
		value = value[:len(value)-1]
	}
	return value
}

func trimTrailingPathSeparators(value string) string {
	for len(value) > 1 && strings.HasSuffix(value, "/") && !isDriveRoot(value) {
		value = strings.TrimSuffix(value, "/")
	}
	return value
}

type pathVariant struct {
	match       string
	replacement string
}

func collapsePathSpans(value string, replacement pathReplacement) string {
	variants := []pathVariant{
		{match: replacement.native, replacement: replacement.placeholder},
		{match: replacement.portable, replacement: replacement.placeholder},
	}
	seen := make(map[string]struct{}, len(variants))
	for _, variant := range variants {
		if variant.match == "" {
			continue
		}
		if _, duplicate := seen[variant.match]; duplicate {
			continue
		}
		seen[variant.match] = struct{}{}
		value = replacePathVariant(value, variant)
	}
	return value
}

func collapseFileURIs(value string, replacements []pathReplacement) string {
	return replaceFileURIs(value, replacements, collapseFileURI)
}

func normalizeFileURIs(value string, replacements []pathReplacement) string {
	return replaceFileURIs(value, replacements, normalizeFileURI)
}

func replaceFileURIs(value string, replacements []pathReplacement, replace func(string, []pathReplacement) (string, bool)) string {
	for offset := 0; ; {
		index := strings.Index(value[offset:], "file://")
		if index < 0 {
			return value
		}
		index += offset
		if !isPathStart(value, index) {
			offset = index + len("file://")
			continue
		}
		end := fileURIEnd(value, index)
		candidate := value[index:end]
		collapsed, ok := replace(candidate, replacements)
		if !ok {
			offset = end
			continue
		}
		value = value[:index] + collapsed + value[end:]
		offset = index + len(collapsed)
	}
}

func normalizeFileURI(candidate string, replacements []pathReplacement) (string, bool) {
	return replaceParsedFileURI(candidate, replacements, func(replacement pathReplacement, tail string) string {
		reference := fileURIReference(replacement.portable)
		tail = escapeURIPath(tail)
		if strings.HasSuffix(reference, "/") && strings.HasPrefix(tail, "/") {
			tail = tail[1:]
		}
		return "file://" + reference + tail
	})
}

func collapseFileURI(candidate string, replacements []pathReplacement) (string, bool) {
	return replaceParsedFileURI(candidate, replacements, func(replacement pathReplacement, tail string) string {
		return "file://" + replacement.placeholder + escapeURIPath(tail)
	})
}

func replaceParsedFileURI(candidate string, replacements []pathReplacement, replace func(pathReplacement, string) string) (string, bool) {
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Scheme != "file" || parsed.User != nil {
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Port() != "" {
		return "", false
	}
	path, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil {
		return "", false
	}
	path = canonicalFixturePath(path)
	switch {
	case host == "" || host == "localhost":
		if len(path) >= 3 && path[0] == '/' && isDrivePath(path[1:]) {
			path = path[1:]
		}
	case host != "":
		path = "//" + host + path
	}
	for _, replacement := range replacements {
		tail, ok := pathRootTail(path, replacement.portable)
		if !ok {
			continue
		}
		collapsed := replace(replacement, tail)
		if parsed.ForceQuery || parsed.RawQuery != "" {
			collapsed += "?" + parsed.RawQuery
		}
		if parsed.Fragment != "" {
			collapsed += "#" + url.PathEscape(parsed.Fragment)
		}
		return collapsed, true
	}
	return "", false
}

func fileURIEnd(value string, start int) int {
	for end := start; end < len(value); end++ {
		character := value[end]
		if isSpace(character) || strings.ContainsRune(",;)]}>\"'`", rune(character)) {
			return end
		}
	}
	return len(value)
}

func fileURIReference(path string) string {
	path = canonicalFixturePath(path)
	if strings.HasPrefix(path, "//") {
		withoutPrefix := strings.TrimPrefix(path, "//")
		host, rest, found := strings.Cut(withoutPrefix, "/")
		if !found {
			return host
		}
		return strings.ToLower(host) + escapeURIPath("/"+rest)
	}
	if isDrivePath(path) {
		path = "/" + path
	}
	return escapeURIPath(path)
}

func escapeURIPath(path string) string {
	return (&url.URL{Path: path}).EscapedPath()
}

func hasPathRoot(path, root string) bool {
	if !strings.HasPrefix(path, root) {
		return false
	}
	if root == "/" {
		return path == root || !strings.HasPrefix(path, "//")
	}
	if isDriveRoot(root) {
		return true
	}
	return len(path) == len(root) || path[len(root)] == '/'
}

func pathRootTail(path, root string) (string, bool) {
	if !hasPathRoot(path, root) {
		return "", false
	}
	tail := strings.TrimPrefix(path, root)
	if tail != "" && (root == "/" || isDriveRoot(root)) {
		tail = "/" + tail
	}
	return tail, true
}

func replacePathVariant(value string, variant pathVariant) string {
	rootMatch := variant.match == "/" || isDriveRoot(portablePath(variant.match))
	for offset := 0; ; {
		index := strings.Index(value[offset:], variant.match)
		if index < 0 {
			return value
		}
		index += offset
		end := index + len(variant.match)
		if !isPathStart(value, index) || (!rootMatch && !isPathEnd(value, end)) || (variant.match == "/" && end < len(value) && value[end] == '/') {
			offset = end
			continue
		}
		tailEnd := pathTailEnd(value, end, rootMatch)
		tail := strings.ReplaceAll(value[end:tailEnd], `\`, "/")
		if rootMatch && tail != "" {
			tail = "/" + tail
		}
		value = value[:index] + variant.replacement + tail + value[tailEnd:]
		offset = index + len(variant.replacement) + len(tail)
	}
}

func isPathStart(value string, index int) bool {
	if index == 0 {
		return true
	}
	previous := value[index-1]
	return isSpace(previous) || strings.ContainsRune("\"'`=:([{<,;", rune(previous))
}

func isPathEnd(value string, end int) bool {
	if end == len(value) {
		return true
	}
	next := value[end]
	return next == '/' || next == '\\' || isSpace(next) || strings.ContainsRune(",;:!?)]}>\"'`", rune(next))
}

func pathTailEnd(value string, start int, root bool) int {
	if start == len(value) || !root && value[start] != '/' && value[start] != '\\' {
		return start
	}
	for end := start; end < len(value); end++ {
		character := value[end]
		if isSpace(character) || strings.ContainsRune(",;:!?)]}>\"'`", rune(character)) {
			return end
		}
	}
	return len(value)
}

func isSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\r' || character == '\n'
}

func isDrivePath(value string) bool {
	return len(value) >= 3 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':' && value[2] == '/'
}

func isDriveRoot(value string) bool {
	return len(value) == 3 && isDrivePath(value)
}

func casePath(dir, name string) (string, error) {
	if dir == "" || name == "" || filepath.IsAbs(name) || strings.Contains(name, `\`) {
		return "", fmt.Errorf("invalid fixture file %q", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("fixture file %q escapes its case", name)
	}
	return filepath.Join(dir, clean), nil
}

func writeAtomic(dir, name string, data []byte) error {
	if _, err := casePath(dir, name); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	name = filepath.Clean(name)
	if err := validateRootPath(root, name, true); err != nil {
		return err
	}
	parent := filepath.Dir(name)
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("create golden nonce: %w", err)
	}
	temporaryName := filepath.Join(parent, ".fixture-"+hex.EncodeToString(nonce[:]))
	temporary, err := root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(temporaryName)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return root.Rename(temporaryName, name)
}

func validateRootPath(root *os.Root, name string, allowMissingFinal bool) error {
	parent := filepath.Dir(name)
	if parent != "." {
		current := ""
		for _, component := range strings.Split(parent, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			info, statErr := root.Lstat(current)
			if statErr != nil {
				return fmt.Errorf("inspect fixture parent %s: %w", component, statErr)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("fixture parent component %q is a symlink", component)
			}
			if !info.IsDir() {
				return fmt.Errorf("fixture parent component %q is not a directory", component)
			}
		}
	}
	if info, statErr := root.Lstat(name); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("fixture file is a symlink")
		}
		if !info.Mode().IsRegular() {
			return errors.New("fixture file is not regular")
		}
	} else if !allowMissingFinal || !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect fixture file: %w", statErr)
	}
	return nil
}

func validateArea(root, area string) []error {
	var problems []error
	if err := validateName("area", area); err != nil {
		return []error{err}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return []error{fmt.Errorf("read fixture area %q: %w", area, err)}
	}
	if len(entries) == 0 {
		return []error{fmt.Errorf("fixture area %q has no cases", area)}
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			problems = append(problems, fmt.Errorf("fixture area %q contains non-case entry %q", area, entry.Name()))
			continue
		}
		id := entry.Name()
		if err := validateName("case ID", id); err != nil {
			problems = append(problems, err)
		}
		dir := filepath.Join(root, id)
		for _, name := range []string{"case.json", "expected.json"} {
			if err := requireRegularFile(dir, name); err != nil {
				problems = append(problems, fmt.Errorf("fixture %s/%s: %w", area, id, err))
			}
		}
		inputFile := isRegularFile(filepath.Join(dir, "input.json"))
		inputDir := isDirectory(filepath.Join(dir, "input"))
		if inputFile == inputDir {
			problems = append(problems, fmt.Errorf("fixture %s/%s must contain exactly one of input.json or input/", area, id))
		}
		warningsPath := filepath.Join(dir, "warnings.json")
		if _, statErr := os.Lstat(warningsPath); statErr == nil && !isRegularFile(warningsPath) {
			problems = append(problems, fmt.Errorf("fixture %s/%s warnings.json is not a regular file", area, id))
		}
		for _, name := range []string{"case.json", "input.json", "expected.json", "warnings.json"} {
			path := filepath.Join(dir, name)
			if !isRegularFile(path) {
				continue
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				problems = append(problems, fmt.Errorf("fixture %s/%s read %s: %w", area, id, name, readErr))
				continue
			}
			if validateErr := validateSortedJSON(data); validateErr != nil {
				problems = append(problems, fmt.Errorf("fixture %s/%s validate %s: %w", area, id, name, validateErr))
			}
		}
		metadataData, readErr := (Case{Area: area, ID: id, Dir: dir}).read("case.json")
		if readErr != nil {
			problems = append(problems, fmt.Errorf("fixture %s/%s decode case.json: %w", area, id, readErr))
		} else {
			var metadata Metadata
			if decodeErr := json.Unmarshal(metadataData, &metadata); decodeErr != nil {
				problems = append(problems, fmt.Errorf("fixture %s/%s decode case.json: %w", area, id, decodeErr))
			} else {
				if metadata.Area != area || metadata.ID != id || metadata.Kind == "" {
					problems = append(problems, fmt.Errorf("fixture %s/%s metadata must name area, id, and a non-empty kind", area, id))
				} else if kindErr := validateKind(metadata.Kind); kindErr != nil {
					problems = append(problems, fmt.Errorf("fixture %s/%s metadata: %w", area, id, kindErr))
				}
			}
		}
		if isRegularFile(warningsPath) {
			warningsData, warningsErr := (Case{Area: area, ID: id, Dir: dir}).read("warnings.json")
			if warningsErr != nil {
				problems = append(problems, fmt.Errorf("fixture %s/%s decode warnings.json: %w", area, id, warningsErr))
			} else if warningsErr := validateWarningsJSON(warningsData); warningsErr != nil {
				problems = append(problems, fmt.Errorf("fixture %s/%s decode warnings.json: %w", area, id, warningsErr))
			}
		}
	}
	return problems
}

func validateWarningsJSON(data []byte) error {
	if err := validateSortedJSON(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var values []WarningFields
	if err := decoder.Decode(&values); err != nil {
		return err
	}
	if values == nil {
		return errors.New("warnings must be a JSON array")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(data, &objects); err != nil {
		return err
	}
	required := [...]string{"code", "line", "path", "source"}
	for index, object := range objects {
		if len(object) != len(required) {
			return fmt.Errorf("warning %d must contain exactly code, line, path, and source", index)
		}
		for _, field := range required {
			if _, ok := object[field]; !ok {
				return fmt.Errorf("warning %d lacks required field %q", index, field)
			}
		}
		if values[index].Code == "" || values[index].Source == "" {
			return fmt.Errorf("warning %d requires non-empty code and source", index)
		}
		if values[index].Line < 0 {
			return fmt.Errorf("warning %d has negative line", index)
		}
		if err := validateWarningPath(values[index].Path); err != nil {
			return fmt.Errorf("warning %d path: %w", index, err)
		}
	}
	return nil
}

func validateWarningPath(value string) error {
	if value == "" {
		return nil
	}
	if strings.Contains(value, `\`) {
		return errors.New("is not slash-separated")
	}
	if strings.HasPrefix(value, "/") {
		return errors.New("must be root-relative")
	}
	if len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':' {
		return errors.New("must not contain a volume or drive")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return errors.New("must be a normalized root-relative path")
		}
	}
	return nil
}

func validateName(label, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("invalid fixture %s %q", label, value)
	}
	for _, character := range value {
		lowercase := character >= 'a' && character <= 'z'
		digit := character >= '0' && character <= '9'
		if lowercase || digit || character == '-' {
			continue
		}
		return fmt.Errorf("invalid fixture %s %q", label, value)
	}
	return nil
}

func validateRunnerName(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return fmt.Errorf("invalid fixture runner %q", value)
	}
	for _, part := range strings.Split(value, "/") {
		if err := validateName("runner", part); err != nil {
			return err
		}
	}
	return nil
}

func validateKind(value string) error {
	if value == "" {
		return errors.New("invalid empty fixture kind")
	}
	for _, character := range value {
		lowercase := character >= 'a' && character <= 'z'
		digit := character >= '0' && character <= '9'
		if lowercase || digit || character == '-' || character == '_' {
			continue
		}
		return fmt.Errorf("invalid fixture kind %q", value)
	}
	return nil
}

func requireRegularFile(dir, name string) error {
	if !isRegularFile(filepath.Join(dir, name)) {
		return fmt.Errorf("lacks regular %s", name)
	}
	return nil
}

func isRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDirectory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
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
