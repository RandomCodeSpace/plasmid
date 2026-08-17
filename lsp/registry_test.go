package lsp

import (
	"slices"
	"testing"

	"github.com/plasmid-dev/plasmid/warning"
)

func TestMergeRegistry(t *testing.T) {
	sink := &warning.SliceSink{}
	registry := MergeRegistry([]Server{
		{ID: "gopls", Command: "custom-gopls", Args: []string{"serve"}},
		{ID: "typescript", Command: "typescript-language-server", Extensions: []string{".TS", ".tsx"}, RootMarkers: []string{"package.json"}},
		{ID: "typescript", Args: []string{"--stdio"}},
		{ID: "bad id", Command: "bad", Extensions: []string{".bad"}},
	}, sink)

	gopls, ok := registry.Server("gopls")
	if !ok || gopls.Command != "custom-gopls" || !slices.Equal(gopls.Extensions, []string{".go"}) {
		t.Fatalf("gopls = %#v, %v", gopls, ok)
	}
	typescript, ok := registry.Server("typescript")
	if !ok || !slices.Equal(typescript.Args, []string{"--stdio"}) || !slices.Equal(typescript.Extensions, []string{".ts", ".tsx"}) {
		t.Fatalf("typescript = %#v, %v", typescript, ok)
	}
	if got := registry.Match("component.TSX"); len(got) != 1 || got[0].ID != "typescript" {
		t.Fatalf("Match = %#v", got)
	}
	warnings := sink.Warnings()
	if len(warnings) != 2 || warnings[0].Code != warning.WarnLSPConfigDuplicateServer || warnings[1].Code != warning.WarnLSPConfigInvalidServer {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestRegistryReturnsDefensiveCopies(t *testing.T) {
	registry := DefaultRegistry()
	server, _ := registry.Server("gopls")
	server.Extensions[0] = ".broken"
	servers := registry.Servers()
	servers[0].RootMarkers[0] = "broken"
	again, _ := registry.Server("gopls")
	if !slices.Equal(again.Extensions, []string{".go"}) || slices.Contains(again.RootMarkers, "broken") {
		t.Fatalf("registry mutated: %#v", again)
	}
}

func TestMergeRegistryCanDisableBuiltin(t *testing.T) {
	registry := MergeRegistry([]Server{{ID: "gopls", Disabled: true}}, warning.DiscardSink{})
	server, ok := registry.Server("gopls")
	if !ok || !server.Disabled || len(registry.Match("main.go")) != 0 {
		t.Fatalf("disabled server = %#v, %v", server, ok)
	}
}

func TestMergeRegistryRejectsNonPortableRootMarker(t *testing.T) {
	sink := &warning.SliceSink{}
	registry := MergeRegistry([]Server{{
		ID: "invalid", Command: "server", Extensions: []string{".go"}, RootMarkers: []string{`dir\marker`},
	}}, sink)
	if _, exists := registry.Server("invalid"); exists {
		t.Fatal("server with non-portable root marker accepted")
	}
	warnings := sink.Warnings()
	if len(warnings) != 1 || warnings[0].Code != warning.WarnLSPConfigInvalidServer {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestMergeRegistryPreservesRootMarkerPrecedence(t *testing.T) {
	registry := MergeRegistry([]Server{{
		ID: "ordered", Command: "server", Extensions: []string{".go"},
		RootMarkers: []string{"go.work", ".git", "go.mod", "go.work"},
	}}, warning.DiscardSink{})
	server, exists := registry.Server("ordered")
	if !exists {
		t.Fatal("ordered server missing")
	}
	if !slices.Equal(server.RootMarkers, []string{"go.work", ".git", "go.mod"}) {
		t.Fatalf("root markers = %#v", server.RootMarkers)
	}
}
