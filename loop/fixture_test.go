package loop

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/plasmid-dev/plasmid/internal/fixture"
)

type loopFixtureMetadata struct {
	Area string `json:"area"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type normalizeFixture struct {
	Coalesce  bool                   `json:"coalesce"`
	Events    []normalizeFixtureItem `json:"events"`
	StopAfter int                    `json:"stopAfter"`
}

type normalizeFixtureItem struct {
	Author string          `json:"author,omitempty"`
	Error  string          `json:"error,omitempty"`
	Final  bool            `json:"final,omitempty"`
	Kind   EventKind       `json:"kind,omitempty"`
	Raw    json.RawMessage `json:"raw,omitempty"`
	Text   string          `json:"text,omitempty"`
}

type normalizeFixtureResult struct {
	Items  []normalizeFixtureItem `json:"items"`
	Pulled int                    `json:"pulled"`
}

type filterFixture struct {
	Allowed    []string `json:"allowed"`
	Disallowed []string `json:"disallowed"`
	Tools      []string `json:"tools"`
}

type filterFixtureResult struct {
	Tools []string `json:"tools"`
}

type defensiveCopyFixture struct {
	Schema   json.RawMessage `json:"schema"`
	Warnings []string        `json:"warnings"`
}

type defensiveCopyResult struct {
	FilterCopy bool `json:"filterCopy"`
	HookCopy   bool `json:"hookCopy"`
	SchemaCopy bool `json:"schemaCopy"`
	SinkCopy   bool `json:"sinkCopy"`
}

type hooksFixture struct {
	After      []string `json:"after"`
	Before     []string `json:"before"`
	MergeLeft  []string `json:"mergeLeft"`
	MergeRight []string `json:"mergeRight"`
}

type hooksFixtureResult struct {
	AfterCalls   []string `json:"afterCalls"`
	AfterResult  string   `json:"afterResult"`
	BeforeCalls  []string `json:"beforeCalls"`
	BeforeResult string   `json:"beforeResult"`
	MergeCalls   []string `json:"mergeCalls"`
}

type warningFixture struct {
	Codes   []string `json:"codes"`
	Warning Warning  `json:"warning"`
}

type warningFixtureResult struct {
	Codes    []string `json:"codes"`
	Rendered string   `json:"rendered"`
}

func init() {
	fixture.Register("loop")
}

func TestLoopFixtureCoverage(t *testing.T) {
	fixture.AssertCoverage(t, "loop")
}

func TestLoopFixtures(t *testing.T) {
	fixture.Walk(t, "loop", func(t *testing.T, testCase fixture.Case) {
		var metadata loopFixtureMetadata
		testCase.Decode(t, "case.json", &metadata)
		if metadata.Area != "loop" || metadata.ID != testCase.ID {
			t.Fatalf("metadata = %#v", metadata)
		}
		switch metadata.Kind {
		case "normalize":
			runNormalizeFixture(t, testCase)
		case "filter-tools":
			runFilterFixture(t, testCase)
		case "defensive-copies":
			runDefensiveCopyFixture(t, testCase)
		case "hooks":
			runHooksFixture(t, testCase)
		case "warning":
			runWarningFixture(t, testCase)
		default:
			t.Fatalf("unknown loop fixture kind %q", metadata.Kind)
		}
	})
}

func runNormalizeFixture(t *testing.T, testCase fixture.Case) {
	t.Helper()
	var input normalizeFixture
	var want normalizeFixtureResult
	testCase.Decode(t, "input.json", &input)
	testCase.Decode(t, "expected.json", &want)
	pulled := 0
	in := func(yield func(Event, error) bool) {
		for _, item := range input.Events {
			pulled++
			var err error
			if item.Error != "" {
				err = errors.New(item.Error)
			}
			if !yield(Event{Kind: item.Kind, Author: item.Author, Text: item.Text, Final: item.Final, Raw: item.Raw}, err) {
				return
			}
		}
	}
	got := normalizeFixtureResult{}
	NormalizeStream(in, input.Coalesce)(func(event Event, err error) bool {
		item := normalizeFixtureItem{Kind: event.Kind, Author: event.Author, Text: event.Text, Final: event.Final}
		item.Raw = event.Raw
		if err != nil {
			item.Error = err.Error()
		}
		got.Items = append(got.Items, item)
		return input.StopAfter == 0 || len(got.Items) < input.StopAfter
	})
	got.Pulled = pulled
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture result mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}

func runFilterFixture(t *testing.T, testCase fixture.Case) {
	t.Helper()
	var input filterFixture
	var want filterFixtureResult
	testCase.Decode(t, "input.json", &input)
	testCase.Decode(t, "expected.json", &want)
	tools := make([]Tool, len(input.Tools))
	for index, name := range input.Tools {
		tools[index] = &stubTool{name: name}
	}
	filtered := FilterTools(tools, View{AllowedTools: input.Allowed, DisallowedTools: input.Disallowed})
	got := filterFixtureResult{Tools: make([]string, len(filtered))}
	for index, tool := range filtered {
		got.Tools[index] = tool.Name()
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture result mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}

func runDefensiveCopyFixture(t *testing.T, testCase fixture.Case) {
	t.Helper()
	var input defensiveCopyFixture
	var want defensiveCopyResult
	testCase.Decode(t, "input.json", &input)
	testCase.Decode(t, "expected.json", &want)

	tool := &stubTool{name: "one", schema: append(json.RawMessage(nil), input.Schema...)}
	schemas := ToolSchemas([]Tool{tool})
	schemas[0].InputSchema[0] = 'x'

	tools := []Tool{tool}
	filtered := FilterTools(tools, View{})
	filtered[0] = &stubTool{name: "changed"}

	hook := BeforeModelHook(func(context.Context, *ModelRequest) (*ModelResponse, error) { return nil, nil })
	left := Hooks{BeforeModel: []BeforeModelHook{hook}}
	merged := left.Merge(Hooks{})
	merged.BeforeModel[0] = nil

	var sink SliceSink
	for _, code := range input.Warnings {
		sink.Warn(Warning{Code: code})
	}
	warnings := sink.Warnings()
	warnings[0].Code = "changed"

	got := defensiveCopyResult{
		FilterCopy: tools[0].Name() == "one",
		HookCopy:   left.BeforeModel[0] != nil,
		SchemaCopy: reflect.DeepEqual(tool.schema, input.Schema),
		SinkCopy:   sink.Warnings()[0].Code == input.Warnings[0],
	}
	if got != want {
		t.Fatalf("fixture result mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}

func runHooksFixture(t *testing.T, testCase fixture.Case) {
	t.Helper()
	var input hooksFixture
	var want hooksFixtureResult
	testCase.Decode(t, "input.json", &input)
	testCase.Decode(t, "expected.json", &want)
	got := hooksFixtureResult{}

	before := make([]BeforeToolHook, 0, len(input.Before))
	for _, label := range input.Before {
		label := label
		before = append(before, func(context.Context, *ToolCall) (*ToolResult, error) {
			got.BeforeCalls = append(got.BeforeCalls, label)
			if label == "short" {
				return &ToolResult{CallID: label}, nil
			}
			return nil, nil
		})
	}
	beforeResult, err := (Hooks{BeforeTool: before}).RunBeforeTool(context.Background(), &ToolCall{})
	if err != nil {
		t.Fatal(err)
	}
	if beforeResult != nil {
		got.BeforeResult = beforeResult.CallID
	}

	after := make([]AfterModelHook, 0, len(input.After))
	for _, label := range input.After {
		label := label
		after = append(after, func(_ context.Context, _ *ModelResponse, _ error) (*ModelResponse, error) {
			got.AfterCalls = append(got.AfterCalls, label)
			return &ModelResponse{Message: Message{Text: label}}, nil
		})
	}
	afterResult, err := (Hooks{AfterModel: after}).RunAfterModel(context.Background(), &ModelResponse{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got.AfterResult = afterResult.Message.Text

	makeMergeHooks := func(labels []string) Hooks {
		hooks := Hooks{}
		for _, label := range labels {
			label := label
			hooks.BeforeModel = append(hooks.BeforeModel, func(context.Context, *ModelRequest) (*ModelResponse, error) {
				got.MergeCalls = append(got.MergeCalls, label)
				return nil, nil
			})
		}
		return hooks
	}
	merged := makeMergeHooks(input.MergeLeft).Merge(makeMergeHooks(input.MergeRight))
	if _, err := merged.RunBeforeModel(context.Background(), &ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture result mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}

func runWarningFixture(t *testing.T, testCase fixture.Case) {
	t.Helper()
	var input warningFixture
	var want warningFixtureResult
	testCase.Decode(t, "input.json", &input)
	testCase.Decode(t, "expected.json", &want)
	got := warningFixtureResult{Codes: append([]string(nil), input.Codes...), Rendered: input.Warning.String()}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture result mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}
