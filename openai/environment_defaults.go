package openai

import (
	_ "unsafe"

	"github.com/openai/openai-go/v3/option"
)

// openAIEnvironmentDefaultsDisabled links to an unsupported internal ABI in
// github.com/openai/openai-go/v3 v3.49.0. google.golang.org/adk/v2 v2.2.0
// constructs the SDK client internally, so no public hook can disable ambient
// OPENAI_* defaults before malformed values are applied. Reevaluate this bridge
// before changing either dependency pin.
//
//go:linkname openAIEnvironmentDefaultsDisabled github.com/openai/openai-go/v3/internal/requestconfig.WithEnvironmentDefaultsDisabled
func openAIEnvironmentDefaultsDisabled() option.RequestOption
