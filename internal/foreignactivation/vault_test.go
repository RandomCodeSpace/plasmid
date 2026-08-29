package foreignactivation_test

import (
	"reflect"
	"testing"

	"github.com/RandomCodeSpace/plasmid/internal/foreignactivation"
)

const (
	changedValue = "changed"
	mutatedValue = "mutated"
)

func TestVaultCapturesDefensiveOneShotDescriptors(t *testing.T) {
	var nilVault *foreignactivation.Vault
	if key := nilVault.Capture(foreignactivation.Descriptor{}); key != "" {
		t.Fatalf("nil vault capture key = %q, want empty", key)
	}
	if _, ok := nilVault.Take("activation-1"); ok {
		t.Fatal("nil vault returned a descriptor")
	}

	vault := &foreignactivation.Vault{}
	original := foreignactivation.Descriptor{
		ID: "server", Transport: "stdio", Command: "server",
		Args: []string{"first"}, Env: map[string]string{"TOKEN": "secret"}, Headers: map[string]string{"X-Test": "value"},
	}
	firstKey := vault.Capture(original)
	secondKey := vault.Capture(foreignactivation.Descriptor{ID: "second"})
	if firstKey != "activation-1" || secondKey != "activation-2" {
		t.Fatalf("keys = %q, %q", firstKey, secondKey)
	}

	original.Args[0] = changedValue
	original.Env["TOKEN"] = changedValue
	original.Headers["X-Test"] = changedValue
	got, ok := vault.Take(firstKey)
	if !ok {
		t.Fatal("captured descriptor was unavailable")
	}
	want := foreignactivation.Descriptor{
		ID: "server", Transport: "stdio", Command: "server",
		Args: []string{"first"}, Env: map[string]string{"TOKEN": "secret"}, Headers: map[string]string{"X-Test": "value"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("descriptor = %#v, want %#v", got, want)
	}

	got.Args[0] = mutatedValue
	got.Env["TOKEN"] = mutatedValue
	got.Headers["X-Test"] = mutatedValue
	if _, ok := vault.Take(firstKey); ok {
		t.Fatal("descriptor was returned more than once")
	}
	if _, ok := vault.Take("missing"); ok {
		t.Fatal("missing descriptor was reported present")
	}
}
