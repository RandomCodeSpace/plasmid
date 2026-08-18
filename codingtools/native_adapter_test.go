package codingtools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

type testNativeHandler func(context.Context, string, map[string]any) (map[string]any, error)

func adaptTestHandler[T any](t *testing.T, handler nativeHandler[T]) testNativeHandler {
	t.Helper()
	var schema jsonschema.Schema
	if err := json.Unmarshal(testSchemaFor[T](), &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, sessionID string, raw map[string]any) (map[string]any, error) {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, err
		}
		var normalized map[string]any
		if err := json.Unmarshal(encoded, &normalized); err != nil {
			return nil, err
		}
		if err := resolved.Validate(normalized); err != nil {
			return nil, err
		}
		var args T
		if err := json.Unmarshal(encoded, &args); err != nil {
			return nil, err
		}
		return handler(ctx, sessionID, args)
	}
}

func testSchemaFor[T any]() json.RawMessage {
	var value T
	switch any(value).(type) {
	case ReadArgs:
		return ReadInputSchema()
	case WriteArgs:
		return WriteInputSchema()
	case EditArgs:
		return EditInputSchema()
	case BashArgs:
		return BashInputSchema()
	case GrepArgs:
		return GrepInputSchema()
	case FindArgs:
		return FindInputSchema()
	case ListArgs:
		return ListInputSchema()
	default:
		panic("unsupported test tool argument type")
	}
}
