package contextresolver

import (
	"github.com/RandomCodeSpace/plasmid/extensions"
	"github.com/RandomCodeSpace/plasmid/internal/syntax"
)

const userScope = "user"

// ExtensionTrust projects normalized extension provenance into the single
// prompt-execution trust model.
func ExtensionTrust(source extensions.Provenance) TrustLevel {
	if source.Scope == userScope || source.Scope == "admin" {
		return TrustUser
	}
	if source.Trusted {
		return TrustRepository
	}
	return TrustUntrusted
}

// ExtensionPolicy projects a normalized extension tool declaration into the
// single turn-scope policy representation.
func ExtensionPolicy(allowed, denied []extensions.ToolPattern, restricted bool) syntax.ToolPolicy {
	allowedPatterns := extensionPatterns(allowed)
	deniedPatterns := extensionPatterns(denied)
	if restricted {
		return syntax.NewRestrictedToolPolicy(allowedPatterns, deniedPatterns)
	}
	return syntax.NewToolPolicy(allowedPatterns, deniedPatterns)
}

func extensionPatterns(values []extensions.ToolPattern) []syntax.ToolPattern {
	result := make([]syntax.ToolPattern, len(values))
	for index, value := range values {
		result[index] = syntax.ToolPattern{Tool: value.Tool, Argument: value.Argument}
	}
	return result
}
