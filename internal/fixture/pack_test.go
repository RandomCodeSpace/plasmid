package fixture

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFixturePackIsByteDeterministic(t *testing.T) {
	root := writePackRepository(t)
	firstManifest, firstArchive, err := buildPack(root)
	if err != nil {
		t.Fatal(err)
	}
	secondManifest, secondArchive, err := buildPack(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstManifest, secondManifest) || !bytes.Equal(firstArchive, secondArchive) {
		t.Fatal("fixture pack generation changed without an input change")
	}

	wantPaths := []string{
		"fixtures/",
		"fixtures/tools/",
		"fixtures/tools/read-basic/",
		"fixtures/tools/read-basic/case.json",
		"fixtures/tools/read-basic/expected.json",
		"fixtures/tools/read-basic/input/",
		"fixtures/tools/read-basic/input/executable",
	}
	assertPackManifest(t, firstManifest, wantPaths)
	assertPackArchive(t, firstArchive, wantPaths)
}

func assertPackManifest(t *testing.T, data []byte, wantPaths []string) {
	t.Helper()
	var manifest packManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != fixturePackVersion || !reflect.DeepEqual(manifest.Areas, []string{"tools"}) {
		t.Fatalf("manifest identity = version %d areas %v", manifest.Version, manifest.Areas)
	}
	gotPaths := make([]string, len(manifest.Entries))
	for index, entry := range manifest.Entries {
		gotPaths[index] = entry.Path
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("manifest paths = %v, want %v", gotPaths, wantPaths)
	}
	if manifest.Entries[len(manifest.Entries)-1].Mode != "0644" {
		t.Fatalf("canonical file mode = %q, want 0644", manifest.Entries[len(manifest.Entries)-1].Mode)
	}
}

func assertPackArchive(t *testing.T, data []byte, wantPaths []string) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(data))
	var archivePaths []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		archivePaths = append(archivePaths, header.Name)
		if !header.ModTime.Equal(time.Unix(0, 0).UTC()) || header.Uid != 0 || header.Gid != 0 {
			t.Fatalf("non-canonical header for %s: %#v", header.Name, header)
		}
	}
	if !reflect.DeepEqual(archivePaths, wantPaths) {
		t.Fatalf("archive paths = %v, want %v", archivePaths, wantPaths)
	}
}

func TestFixturePackUpdateAndReadOnlyVerification(t *testing.T) {
	root := writePackRepository(t)
	if err := UpdatePack(root); err != nil {
		t.Fatal(err)
	}
	outputRoot := filepath.Join(root, "testdata", "conformance")
	t.Cleanup(func() {
		_ = os.Chmod(outputRoot, 0o755)
		_ = os.Chmod(filepath.Join(outputRoot, fixtureManifestName), 0o644)
		_ = os.Chmod(filepath.Join(outputRoot, fixtureArchiveName), 0o644)
	})
	firstManifest := mustReadFile(t, filepath.Join(outputRoot, fixtureManifestName))
	firstArchive := mustReadFile(t, filepath.Join(outputRoot, fixtureArchiveName))
	if err := UpdatePack(root); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstManifest, mustReadFile(t, filepath.Join(outputRoot, fixtureManifestName))) ||
		!bytes.Equal(firstArchive, mustReadFile(t, filepath.Join(outputRoot, fixtureArchiveName))) {
		t.Fatal("clean fixture regeneration changed artifact bytes")
	}
	for _, name := range []string{fixtureManifestName, fixtureArchiveName} {
		if err := os.Chmod(filepath.Join(outputRoot, name), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(outputRoot, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPack(root); err != nil {
		t.Fatalf("read-only verification failed: %v", err)
	}
}

func TestFixturePackVerificationDetectsDrift(t *testing.T) {
	root := writePackRepository(t)
	if err := UpdatePack(root); err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(root, "testdata", "fixtures", "tools", "read-basic", "expected.json")
	writeTestFile(t, expected, "{\"ok\":false}\n")
	if err := VerifyPack(root); err == nil || !strings.Contains(err.Error(), "has drifted") {
		t.Fatalf("VerifyPack() error = %v, want drift", err)
	}
	if err := UpdatePack(root); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "testdata", "conformance", fixtureManifestName)
	writeTestFile(t, manifest, "{}\n")
	if err := VerifyPack(root); err == nil || !strings.Contains(err.Error(), fixtureManifestName) {
		t.Fatalf("VerifyPack() error = %v, want manifest drift", err)
	}
}

func TestPublicFixturePackReportsMissingAndUnsafeArtifacts(t *testing.T) {
	for _, test := range []struct {
		name, shape, want string
		update            bool
	}{
		{name: "missing artifact", shape: "missing", want: "inspect fixture artifact"},
		{name: "nonregular artifact", shape: "directory", want: "not a regular file"},
		{name: "symlinked output", shape: "output-symlink", want: "not a directory", update: true},
		{name: "symlinked artifact", shape: "artifact-symlink", want: "not a regular file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := preparePackArtifactProblem(t, test.shape)
			err := VerifyPack(root)
			if test.update {
				err = UpdatePack(root)
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("fixture pack error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func preparePackArtifactProblem(t *testing.T, shape string) string {
	t.Helper()
	root := writePackRepository(t)
	if shape == "output-symlink" {
		mustSymlinkPath(t, "fixtures", filepath.Join(root, "testdata", "conformance"))
		return root
	}
	if shape == "missing" {
		return root
	}
	if err := UpdatePack(root); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "testdata", "conformance", fixtureArchiveName)
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if shape == "directory" {
		mustMkdirPath(t, archive)
	} else {
		mustSymlinkPath(t, fixtureManifestName, archive)
	}
	return root
}

func TestPublicFixturePackRejectsInvalidRepositoryShapes(t *testing.T) {
	tests := []struct {
		name  string
		shape string
		want  string
	}{
		{name: "missing fixture root", shape: "missing", want: "inspect fixture root"},
		{name: "fixture root is a file", shape: "file", want: "fixture root is not a directory"},
		{name: "fixture root is a symlink", shape: "fixture-symlink", want: "fixture root is not a directory"},
		{name: "fixture root has no areas", shape: "empty", want: "fixture root has no areas"},
		{name: "fixture root contains a file", shape: "non-area", want: "non-area entry"},
		{name: "fixture area name is invalid", shape: "invalid-area", want: "invalid fixture area"},
		{name: "output path is a file", shape: "output-file", want: "output is not a directory"},
		{name: "fixture pack parent is a symlink", shape: "parent-symlink", want: "parent is not a directory"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := prepareInvalidPackRepository(t, test.shape)
			if err := UpdatePack(root); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("UpdatePack() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func prepareInvalidPackRepository(t *testing.T, shape string) string {
	t.Helper()
	root := t.TempDir()
	testdata := filepath.Join(root, "testdata")
	fixtures := filepath.Join(testdata, "fixtures")
	switch shape {
	case "missing":
	case "file":
		mustMkdirPath(t, testdata)
		writeTestFile(t, fixtures, "not a directory")
	case "fixture-symlink":
		mustMkdirPath(t, testdata)
		mustSymlinkPath(t, t.TempDir(), fixtures)
	case "empty":
		mustMkdirPath(t, fixtures)
	case "non-area":
		mustMkdirPath(t, fixtures)
		writeTestFile(t, filepath.Join(fixtures, "README"), "not an area")
	case "invalid-area":
		mustMkdirPath(t, filepath.Join(fixtures, "Bad Area"))
	case "output-file":
		seedPackRepositoryAt(t, testdata)
		writeTestFile(t, filepath.Join(testdata, "conformance"), "not a directory")
	case "parent-symlink":
		target := t.TempDir()
		seedPackRepositoryAt(t, target)
		mustSymlinkPath(t, target, testdata)
	default:
		t.Fatalf("unknown invalid fixture pack shape %q", shape)
	}
	return root
}

func mustMkdirPath(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustSymlinkPath(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

func TestFixturePackValidatesMetadataAndRejectsSymlinks(t *testing.T) {
	for _, test := range []struct{ name, shape, want string }{
		{name: "metadata", shape: "metadata", want: "metadata must name area"},
		{name: "kind", shape: "kind", want: "invalid fixture kind"},
		{name: "symlink", shape: "symlink", want: "rejects symlink"},
		{name: "separator alias", shape: "separator", want: "non-portable character"},
		{name: "case-insensitive collision", shape: "collision", want: "case-insensitive path collision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := prepareInvalidPackEntry(t, test.shape)
			if err := UpdatePack(root); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("UpdatePack() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func prepareInvalidPackEntry(t *testing.T, shape string) string {
	t.Helper()
	root := writePackRepository(t)
	caseRoot := filepath.Join(root, "testdata", "fixtures", "tools", "read-basic")
	metadata := filepath.Join(caseRoot, caseMetadataName)
	input := filepath.Join(caseRoot, "input")
	switch shape {
	case "metadata":
		writeTestFile(t, metadata, "{\"area\":\"wrong\",\"id\":\"read-basic\",\"kind\":\"read\"}\n")
	case "kind":
		writeTestFile(t, metadata, "{\"area\":\"tools\",\"id\":\"read-basic\",\"kind\":\"Bad Kind\"}\n")
	case "symlink":
		mustSymlinkPath(t, "executable", filepath.Join(input, "linked"))
	case "separator":
		requireNonWindowsPackPath(t, "literal backslash")
		writeTestFile(t, filepath.Join(input, `bad\name`), "bad\n")
	case "collision":
		requireNonWindowsPackPath(t, "case-distinct files")
		writeTestFile(t, filepath.Join(input, "NAME"), "upper\n")
		writeTestFile(t, filepath.Join(input, "name"), "lower\n")
	}
	return root
}

func requireNonWindowsPackPath(t *testing.T, feature string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skipf("Windows cannot create %s", feature)
	}
}

func TestPortablePackComponents(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "plain.txt"},
		{name: `back\slash`, want: "non-portable character"},
		{name: "bad:name", want: "non-portable character"},
		{name: "control\x1f", want: "non-portable character"},
		{name: "trailing.", want: "trailing dot or space"},
		{name: "trailing ", want: "trailing dot or space"},
		{name: "CON", want: "reserved Windows device"},
		{name: "con.json", want: "reserved Windows device"},
		{name: "Lpt9.txt", want: "reserved Windows device"},
		{name: "COM\u00b9.txt", want: "reserved Windows device"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validatePortableComponent(test.name)
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("validatePortableComponent() error = %v, want %q", err, test.want)
			}
		})
	}
	if err := validatePortableSiblings([]string{"input", "Input"}); err == nil || !strings.Contains(err.Error(), "case-insensitive") {
		t.Fatalf("validatePortableSiblings() error = %v", err)
	}
}

func writePackRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	seedPackRepositoryAt(t, filepath.Join(root, "testdata"))
	return root
}

func seedPackRepositoryAt(t *testing.T, testdata string) {
	t.Helper()
	caseRoot := filepath.Join(testdata, "fixtures", "tools", "read-basic")
	if err := os.MkdirAll(filepath.Join(caseRoot, "input"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(caseRoot, "case.json"), "{\"area\":\"tools\",\"id\":\"read-basic\",\"kind\":\"read\"}\n")
	writeTestFile(t, filepath.Join(caseRoot, "expected.json"), "{\"ok\":true}\n")
	executable := filepath.Join(caseRoot, "input", "executable")
	writeTestFile(t, executable, "fixture\n")
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
