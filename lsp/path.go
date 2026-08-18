package lsp

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"go.lsp.dev/protocol"
)

var (
	// ErrInvalidURI indicates a non-file or malformed document URI.
	ErrInvalidURI = errors.New("invalid LSP file URI")
	// ErrInvalidPosition indicates a byte offset or LSP position that does not
	// identify a Unicode boundary in the supplied document.
	ErrInvalidPosition = errors.New("invalid LSP position")
)

// PathToFileURI converts an absolute native or Windows path to a file URI.
func PathToFileURI(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", ErrInvalidURI
	}
	if isWindowsDrivePath(path) {
		slashed := strings.ReplaceAll(path, `\`, "/")
		return (&url.URL{Scheme: "file", Path: "/" + slashed}).String(), nil
	}
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		slashed := strings.TrimLeft(strings.ReplaceAll(path, `\`, "/"), "/")
		host, rest, found := strings.Cut(slashed, "/")
		if !found || host == "" || rest == "" {
			return "", ErrInvalidURI
		}
		return (&url.URL{Scheme: "file", Host: host, Path: "/" + rest}).String(), nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path to file URI: %w", ErrInvalidURI)
	}
	absolute, err := filepath.Abs(path)
	if err != nil || !filepath.IsAbs(absolute) {
		return "", fmt.Errorf("path to file URI: %w", ErrInvalidURI)
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String(), nil
}

// FileURIToPath converts a file URI to a native path. Drive-letter URIs remain
// recognizable on non-Windows hosts so portable fixture data can be decoded.
func FileURIToPath(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || !strings.EqualFold(parsed.Scheme, "file") || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalidURI
	}
	if strings.ContainsRune(parsed.Path, 0) || strings.ContainsRune(parsed.Host, 0) {
		return "", ErrInvalidURI
	}
	path := parsed.Path
	if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
		path = "//" + parsed.Host + "/" + strings.TrimLeft(path, "/")
	}
	if len(path) >= 3 && path[0] == '/' && isASCIIAlpha(path[1]) && path[2] == ':' {
		path = path[1:]
	}
	if path == "" {
		return "", ErrInvalidURI
	}
	if runtime.GOOS == "windows" {
		path = filepath.FromSlash(path)
	}
	return path, nil
}

func isWindowsDrivePath(path string) bool {
	return len(path) >= 3 && isASCIIAlpha(path[0]) && path[1] == ':' && (path[2] == '/' || path[2] == '\\')
}

func isASCIIAlpha(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

// PositionForOffset converts a byte offset to the requested LSP encoding.
func PositionForOffset(content []byte, offset int, encoding protocol.PositionEncodingKind) (protocol.Position, error) {
	if offset < 0 || offset > len(content) || !utf8.Valid(content) || offset < len(content) && !utf8.RuneStart(content[offset]) {
		return protocol.Position{}, ErrInvalidPosition
	}
	lineStart := 0
	line := uint32(0)
	for index := 0; index < offset; index++ {
		if content[index] == '\n' {
			line++
			lineStart = index + 1
		}
	}
	if offset > 0 && offset < len(content) && content[offset-1] == '\r' && content[offset] == '\n' {
		return protocol.Position{}, ErrInvalidPosition
	}
	character, err := encodedLength(content[lineStart:offset], encoding)
	if err != nil {
		return protocol.Position{}, err
	}
	return protocol.Position{Line: line, Character: character}, nil
}

// OffsetForPosition converts an LSP position to a byte offset.
func OffsetForPosition(content []byte, position protocol.Position, encoding protocol.PositionEncodingKind) (int, error) {
	if !utf8.Valid(content) {
		return 0, ErrInvalidPosition
	}
	lineStart := 0
	for line := uint32(0); line < position.Line; line++ {
		newline := bytes.IndexByte(content[lineStart:], '\n')
		if newline < 0 {
			return 0, ErrInvalidPosition
		}
		lineStart += newline + 1
	}
	lineEnd := lineStart
	for lineEnd < len(content) && content[lineEnd] != '\n' {
		lineEnd++
	}
	if lineEnd > lineStart && content[lineEnd-1] == '\r' {
		lineEnd--
	}
	wanted := position.Character
	used := uint32(0)
	for offset := lineStart; offset < lineEnd; {
		if used == wanted {
			return offset, nil
		}
		runeValue, width := utf8.DecodeRune(content[offset:lineEnd])
		units, err := encodedRuneLength(runeValue, width, encoding)
		if err != nil || used+units > wanted {
			return 0, ErrInvalidPosition
		}
		used += units
		offset += width
	}
	if used == wanted {
		return lineEnd, nil
	}
	return 0, ErrInvalidPosition
}

func encodedLength(content []byte, encoding protocol.PositionEncodingKind) (uint32, error) {
	if encoding == "" {
		encoding = protocol.PositionEncodingKindUTF16
	}
	switch encoding {
	case protocol.PositionEncodingKindUTF8:
		return uint32(len(content)), nil
	case protocol.PositionEncodingKindUTF16:
		var length uint32
		for len(content) != 0 {
			runeValue, width := utf8.DecodeRune(content)
			length += uint32(len(utf16.Encode([]rune{runeValue})))
			content = content[width:]
		}
		return length, nil
	default:
		return 0, ErrInvalidPosition
	}
}

func encodedRuneLength(runeValue rune, width int, encoding protocol.PositionEncodingKind) (uint32, error) {
	if encoding == "" {
		encoding = protocol.PositionEncodingKindUTF16
	}
	switch encoding {
	case protocol.PositionEncodingKindUTF8:
		return uint32(width), nil
	case protocol.PositionEncodingKindUTF16:
		if runeValue > 0xffff {
			return 2, nil
		}
		return 1, nil
	default:
		return 0, ErrInvalidPosition
	}
}
