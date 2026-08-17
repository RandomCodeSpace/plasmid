// Package foreignactivation carries consent-gated MCP runtime descriptors
// between Plasmid packages without adding them to the public foreign catalog.
package foreignactivation

import "strconv"

// Descriptor contains secret-bearing runtime configuration. It must never be
// serialized into normalized catalogs, warnings, or fixtures.
type Descriptor struct {
	ID, Transport, Command, URL string
	Args                        []string
	Env, Headers                map[string]string
}

// Vault is an internal one-way capability used by the extension catalog.
type Vault struct {
	next   uint64
	values map[string]Descriptor
}

// Capture retains a defensive descriptor copy.
func (v *Vault) Capture(value Descriptor) string {
	if v == nil {
		return ""
	}
	if v.values == nil {
		v.values = make(map[string]Descriptor)
	}
	v.next++
	key := "activation-" + strconv.FormatUint(v.next, 10)
	value.Args = append([]string(nil), value.Args...)
	value.Env = cloneStrings(value.Env)
	value.Headers = cloneStrings(value.Headers)
	v.values[key] = value
	return key
}

// Take returns a defensive copy and removes the selected descriptor.
func (v *Vault) Take(key string) (Descriptor, bool) {
	if v == nil {
		return Descriptor{}, false
	}
	value, ok := v.values[key]
	if !ok {
		return Descriptor{}, false
	}
	delete(v.values, key)
	value.Args = append([]string(nil), value.Args...)
	value.Env = cloneStrings(value.Env)
	value.Headers = cloneStrings(value.Headers)
	return value, true
}

func cloneStrings(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}
