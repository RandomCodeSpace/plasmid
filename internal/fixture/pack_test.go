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

	var manifest packManifest
	if err := json.Unmarshal(firstManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != fixturePackVersion || !reflect.DeepEqual(manifest.Areas, []string{"tools"}) {
		t.Fatalf("manifest identity = version %d areas %v", manifest.Version, manifest.Areas)
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

	reader := tar.NewReader(bytes.NewReader(firstArchive))
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
	t.Run("missing artifact", func(t *testing.T) {
		root := writePackRepository(t)
		if err := VerifyPack(root); err == nil || !strings.Contains(err.Error(), "inspect fixture artifact") {
			t.Fatalf("VerifyPack() error = %v, want missing artifact", err)
		}
	})
	t.Run("nonregular artifact", func(t *testing.T) {
		root := writePackRepository(t)
		if err := UpdatePack(root); err != nil {
			t.Fatal(err)
		}
		archive := filepath.Join(root, "testdata", "conformance", fixtureArchiveName)
		if err := os.Remove(archive); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(archive, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := VerifyPack(root); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("VerifyPack() error = %v, want non-regular artifact", err)
		}
	})
	t.Run("symlinked output", func(t *testing.T) {
		root := writePackRepository(t)
		output := filepath.Join(root, "testdata", "conformance")
		if err := os.Symlink("fixtures", output); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if err := UpdatePack(root); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("UpdatePack() error = %v, want unsafe output", err)
		}
	})
}

func TestFixturePackValidatesMetadataAndRejectsSymlinks(t *testing.T) {
	t.Run("metadata", func(t *testing.T) {
		root := writePackRepository(t)
		metadata := filepath.Join(root, "testdata", "fixtures", "tools", "read-basic", "case.json")
		writeTestFile(t, metadata, "{\"area\":\"wrong\",\"id\":\"read-basic\",\"kind\":\"read\"}\n")
		if _, _, err := buildPack(root); err == nil || !strings.Contains(err.Error(), "metadata must name area") {
			t.Fatalf("buildPack() error = %v", err)
		}
	})
	t.Run("kind", func(t *testing.T) {
		root := writePackRepository(t)
		metadata := filepath.Join(root, "testdata", "fixtures", "tools", "read-basic", "case.json")
		writeTestFile(t, metadata, "{\"area\":\"tools\",\"id\":\"read-basic\",\"kind\":\"Bad Kind\"}\n")
		if _, _, err := buildPack(root); err == nil || !strings.Contains(err.Error(), "invalid fixture kind") {
			t.Fatalf("buildPack() error = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := writePackRepository(t)
		link := filepath.Join(root, "testdata", "fixtures", "tools", "read-basic", "input", "linked")
		if err := os.Symlink("executable", link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, _, err := buildPack(root); err == nil || !strings.Contains(err.Error(), "rejects symlink") {
			t.Fatalf("buildPack() error = %v", err)
		}
	})
	t.Run("separator alias", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows cannot create a literal backslash path component")
		}
		root := writePackRepository(t)
		path := filepath.Join(root, "testdata", "fixtures", "tools", "read-basic", "input", `bad\name`)
		writeTestFile(t, path, "bad\n")
		if _, _, err := buildPack(root); err == nil || !strings.Contains(err.Error(), "non-portable character") {
			t.Fatalf("buildPack() error = %v", err)
		}
	})
	t.Run("case-insensitive collision", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows cannot create case-distinct colliding files")
		}
		root := writePackRepository(t)
		input := filepath.Join(root, "testdata", "fixtures", "tools", "read-basic", "input")
		writeTestFile(t, filepath.Join(input, "NAME"), "upper\n")
		writeTestFile(t, filepath.Join(input, "name"), "lower\n")
		if _, _, err := buildPack(root); err == nil || !strings.Contains(err.Error(), "case-insensitive path collision") {
			t.Fatalf("buildPack() error = %v", err)
		}
	})
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
	caseRoot := filepath.Join(root, "testdata", "fixtures", "tools", "read-basic")
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
	return root
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
