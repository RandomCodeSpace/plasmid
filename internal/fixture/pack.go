package fixture

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	fixtureArchiveName  = "fixtures-v1.tar"
	fixtureManifestName = "manifest-v1.json"
	fixturePackVersion  = 1
)

type packArchive struct {
	Format string `json:"format"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type packManifest struct {
	Archive packArchive         `json:"archive"`
	Areas   []string            `json:"areas"`
	Entries []packManifestEntry `json:"entries"`
	Version int                 `json:"version"`
}

type packManifestEntry struct {
	Mode   string `json:"mode"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Type   string `json:"type"`
}

type packEntry struct {
	data []byte
	packManifestEntry
}

// UpdatePack regenerates the language-neutral fixture archive and manifest.
func UpdatePack(root string) error {
	manifest, archive, err := buildPack(root)
	if err != nil {
		return err
	}
	outputRoot := filepath.Join(root, "testdata", "conformance")
	if err := ensurePackOutputRoot(outputRoot); err != nil {
		return err
	}
	if err := writeAtomic(outputRoot, fixtureArchiveName, archive); err != nil {
		return fmt.Errorf("write fixture archive: %w", err)
	}
	if err := writeAtomic(outputRoot, fixtureManifestName, manifest); err != nil {
		return fmt.Errorf("write fixture manifest: %w", err)
	}
	return nil
}

// VerifyPack compares the committed fixture archive and manifest without
// mutating either the fixtures or their generated artifacts.
func VerifyPack(root string) error {
	manifest, archive, err := buildPack(root)
	if err != nil {
		return err
	}
	outputRoot := filepath.Join(root, "testdata", "conformance")
	for _, artifact := range []struct {
		data []byte
		name string
	}{
		{data: archive, name: fixtureArchiveName},
		{data: manifest, name: fixtureManifestName},
	} {
		path := filepath.Join(outputRoot, artifact.name)
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return fmt.Errorf("inspect fixture artifact %s: %w", artifact.name, statErr)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("fixture artifact %s is not a regular file", artifact.name)
		}
		committed, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read fixture artifact %s: %w", artifact.name, readErr)
		}
		if !bytes.Equal(committed, artifact.data) {
			return fmt.Errorf("fixture artifact %s has drifted; regenerate the fixture pack", artifact.name)
		}
	}
	return nil
}

func buildPack(root string) ([]byte, []byte, error) {
	entries, areas, err := collectPackEntries(filepath.Join(root, "testdata", "fixtures"))
	if err != nil {
		return nil, nil, err
	}
	archive, err := buildArchive(entries)
	if err != nil {
		return nil, nil, err
	}
	manifestEntries := make([]packManifestEntry, len(entries))
	for index, entry := range entries {
		manifestEntries[index] = entry.packManifestEntry
	}
	archiveHash := sha256.Sum256(archive)
	manifest := packManifest{
		Archive: packArchive{
			Format: "tar",
			Path:   fixtureArchiveName,
			SHA256: hex.EncodeToString(archiveHash[:]),
			Size:   int64(len(archive)),
		},
		Areas:   areas,
		Entries: manifestEntries,
		Version: fixturePackVersion,
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("encode fixture manifest: %w", err)
	}
	return append(encoded, '\n'), archive, nil
}

func collectPackEntries(fixturesRoot string) ([]packEntry, []string, error) {
	rootInfo, err := os.Lstat(fixturesRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect fixture root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("fixture root is not a directory")
	}
	areaEntries, err := os.ReadDir(fixturesRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("read fixture root: %w", err)
	}
	areas := make([]string, 0, len(areaEntries))
	var problems []error
	for _, areaEntry := range areaEntries {
		if areaEntry.Type()&os.ModeSymlink != 0 || !areaEntry.IsDir() {
			problems = append(problems, fmt.Errorf("fixture root contains non-area entry %q", areaEntry.Name()))
			continue
		}
		area := areaEntry.Name()
		if nameErr := validateName("area", area); nameErr != nil {
			problems = append(problems, nameErr)
			continue
		}
		areas = append(areas, area)
		problems = append(problems, validateArea(filepath.Join(fixturesRoot, area), area)...)
	}
	if len(areaEntries) == 0 {
		problems = append(problems, errors.New("fixture root has no areas"))
	}
	if err := errors.Join(problems...); err != nil {
		return nil, nil, err
	}
	sort.Strings(areas)

	entries := []packEntry{{packManifestEntry: packManifestEntry{
		Mode: "0755", Path: "fixtures/", SHA256: "", Size: 0, Type: "directory",
	}}}
	siblings := make(map[string][]string)
	err = filepath.WalkDir(fixturesRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == fixturesRoot {
			return nil
		}
		if componentErr := validatePortableComponent(entry.Name()); componentErr != nil {
			return fmt.Errorf("fixture pack path component %q: %w", entry.Name(), componentErr)
		}
		parent := filepath.Dir(path)
		names := append(siblings[parent], entry.Name())
		if siblingErr := validatePortableSiblings(names); siblingErr != nil {
			return fmt.Errorf("fixture pack directory %s: %w", parent, siblingErr)
		}
		siblings[parent] = names
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture pack rejects symlink %s", path)
		}
		relative, relativeErr := filepath.Rel(fixturesRoot, path)
		if relativeErr != nil {
			return fmt.Errorf("resolve fixture pack path: %w", relativeErr)
		}
		portable := "fixtures/" + filepath.ToSlash(relative)
		if entry.IsDir() {
			entries = append(entries, packEntry{packManifestEntry: packManifestEntry{
				Mode: "0755", Path: portable + "/", SHA256: "", Size: 0, Type: "directory",
			}})
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("inspect fixture pack entry %s: %w", portable, infoErr)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("fixture pack entry %s is not a regular file", portable)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read fixture pack entry %s: %w", portable, readErr)
		}
		hash := sha256.Sum256(data)
		entries = append(entries, packEntry{data: data, packManifestEntry: packManifestEntry{
			Mode: "0644", Path: portable, SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data)), Type: "file",
		}})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, areas, nil
}

func validatePortableComponent(name string) error {
	if name == "" || name == "." || name == ".." {
		return errors.New("is not a portable relative component")
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return errors.New("has a trailing dot or space")
	}
	for _, character := range name {
		if character < 32 || strings.ContainsRune(`<>:"/\|?*`, character) {
			return fmt.Errorf("contains non-portable character %q", character)
		}
	}
	base := name
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	upper := strings.ToUpper(base)
	switch upper {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$", "CLOCK$":
		return errors.New("uses a reserved Windows device name")
	}
	port := []rune(upper)
	if len(port) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && isReservedWindowsPortDigit(port[3]) {
		return errors.New("uses a reserved Windows device name")
	}
	return nil
}

func isReservedWindowsPortDigit(character rune) bool {
	return character >= '1' && character <= '9' || character == '\u00b9' || character == '\u00b2' || character == '\u00b3'
}

func validatePortableSiblings(names []string) error {
	for index, name := range names {
		for _, previous := range names[:index] {
			if strings.EqualFold(previous, name) {
				return fmt.Errorf("case-insensitive path collision between %q and %q", previous, name)
			}
		}
	}
	return nil
}

func buildArchive(entries []packEntry) ([]byte, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		mode := int64(0o644)
		typeflag := byte(tar.TypeReg)
		if entry.Type == "directory" {
			mode = 0o755
			typeflag = tar.TypeDir
		}
		header := &tar.Header{
			Format:   tar.FormatUSTAR,
			Mode:     mode,
			ModTime:  time.Unix(0, 0).UTC(),
			Name:     entry.Path,
			Size:     entry.Size,
			Typeflag: typeflag,
		}
		if err := writer.WriteHeader(header); err != nil {
			writer.Close()
			return nil, fmt.Errorf("write fixture archive header %s: %w", entry.Path, err)
		}
		if entry.Type == "file" {
			if _, err := writer.Write(entry.data); err != nil {
				writer.Close()
				return nil, fmt.Errorf("write fixture archive data %s: %w", entry.Path, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close fixture archive: %w", err)
	}
	return buffer.Bytes(), nil
}

func ensurePackOutputRoot(path string) error {
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect fixture pack parent: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("fixture pack parent is not a directory")
	}
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create fixture pack directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect fixture pack directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("fixture pack output is not a directory")
	}
	return nil
}
