package codingtools

import (
	"context"
	"fmt"
	"sort"

	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

// MaxTouchEvents bounds search fan-out into context and LSP observers.
const MaxTouchEvents = 256

func publishSearchTouches(ctx context.Context, bus *workspace.TouchBus, warnings warning.Sink, sessionID string, paths []string) {
	if len(paths) == 0 {
		return
	}
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	deduplicated := paths[:0]
	for _, path := range paths {
		if len(deduplicated) == 0 || deduplicated[len(deduplicated)-1] != path {
			deduplicated = append(deduplicated, path)
		}
	}
	if len(deduplicated) > MaxTouchEvents {
		if warnings == nil {
			warnings = warning.SlogSink{}
		}
		warnings.Warn(warning.Warning{
			Code:    warning.WarnContextTouchOverflow,
			Source:  "codingtools",
			Message: fmt.Sprintf("search touch events capped at %d; %d matched paths omitted", MaxTouchEvents, len(deduplicated)-MaxTouchEvents),
		})
		deduplicated = deduplicated[:MaxTouchEvents]
	}
	for _, path := range deduplicated {
		bus.Publish(ctx, workspace.Touch{SessionID: sessionID, InvocationID: invocationID(ctx), Path: path, Kind: workspace.TouchSearch})
	}
}

func resultLimit(grant, configured int) int {
	if grant <= 0 {
		return 0
	}
	if configured <= 0 || grant < configured {
		return grant
	}
	return configured
}
