package lsp

import (
	"errors"
	"runtime"
	"testing"

	"go.lsp.dev/protocol"
)

func TestFileURIConversions(t *testing.T) {
	tests := []struct {
		name string
		path string
		uri  string
	}{
		{name: "unix escaping", path: "/tmp/a b#c.go", uri: "file:///tmp/a%20b%23c.go"},
		{name: "windows drive", path: `C:\Users\dev\a b.go`, uri: "file:///C:/Users/dev/a%20b.go"},
		{name: "unc", path: `\\server\share\a.go`, uri: "file://server/share/a.go"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := PathToFileURI(test.path)
			if err != nil || got != test.uri {
				t.Fatalf("PathToFileURI = %q, %v; want %q", got, err, test.uri)
			}
			decoded, err := FileURIToPath(got)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && test.name == "windows drive" && decoded != "C:/Users/dev/a b.go" {
				t.Fatalf("decoded drive path = %q", decoded)
			}
		})
	}
	for _, value := range []string{"https://example.com/a.go", "file:///a.go?x=1", "file://", "file:///%ZZ"} {
		if _, err := FileURIToPath(value); !errors.Is(err, ErrInvalidURI) {
			t.Errorf("FileURIToPath(%q) error = %v", value, err)
		}
	}
	if _, err := PathToFileURI("relative.go"); !errors.Is(err, ErrInvalidURI) {
		t.Fatalf("relative path error = %v", err)
	}
}

func TestPositionConversion(t *testing.T) {
	content := []byte("a😀b\r\néx\n")
	tests := []struct {
		name     string
		offset   int
		encoding protocol.PositionEncodingKind
		want     protocol.Position
	}{
		{name: "utf16 before emoji", offset: 1, encoding: protocol.PositionEncodingKindUTF16, want: protocol.Position{Line: 0, Character: 1}},
		{name: "utf16 after emoji", offset: 5, encoding: protocol.PositionEncodingKindUTF16, want: protocol.Position{Line: 0, Character: 3}},
		{name: "utf8 after emoji", offset: 5, encoding: protocol.PositionEncodingKindUTF8, want: protocol.Position{Line: 0, Character: 5}},
		{name: "second line", offset: 10, encoding: protocol.PositionEncodingKindUTF16, want: protocol.Position{Line: 1, Character: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			position, err := PositionForOffset(content, test.offset, test.encoding)
			if err != nil || position != test.want {
				t.Fatalf("PositionForOffset = %#v, %v; want %#v", position, err, test.want)
			}
			offset, err := OffsetForPosition(content, position, test.encoding)
			if err != nil || offset != test.offset {
				t.Fatalf("OffsetForPosition = %d, %v; want %d", offset, err, test.offset)
			}
		})
	}
	if _, err := PositionForOffset(content, 2, protocol.PositionEncodingKindUTF16); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("mid-rune offset error = %v", err)
	}
	if _, err := PositionForOffset(content, 7, protocol.PositionEncodingKindUTF16); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("mid-CRLF offset error = %v", err)
	}
	if _, err := OffsetForPosition(content, protocol.Position{Line: 0, Character: 2}, protocol.PositionEncodingKindUTF16); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("mid-surrogate position error = %v", err)
	}
}
