package oneshot

import (
	"context"
	"errors"
	"reflect"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const (
	probeToolName             = "plasmid_ping"
	probeMarkerName           = "marker"
	probeMarkerValue          = "plasmid-probe-v1"
	probeMaxOutputTokens      = int32(64)
	probeMaxReturnedTextBytes = 1024
)

// ProbeRequest contains the model and output-token budget for one tool-calling
// capability probe. MaxOutputTokens must be positive.
type ProbeRequest struct {
	Model           model.LLM
	MaxOutputTokens int32
}

// ProbeToolCalling makes one direct, non-streaming model request and succeeds
// only when the model returns the advertised inert ping call. It does not
// construct a runner, session, or executable tool. It uses a 64-token output
// budget. Use Probe when the host needs a different positive budget.
func ProbeToolCalling(ctx context.Context, llm model.LLM) (Result, error) {
	return Probe(ctx, ProbeRequest{Model: llm, MaxOutputTokens: probeMaxOutputTokens})
}

// Probe makes one direct, non-streaming model request with the supplied
// positive output-token budget and succeeds only when the model returns the
// advertised inert ping call. It does not construct a runner, session, or
// executable tool.
func Probe(ctx context.Context, request ProbeRequest) (Result, error) {
	var result Result
	if ctx == nil {
		return Result{}, codedError(CodeInvalidArgument, "probe tool calling", ErrInvalidArgument, errors.New("context is nil"))
	}
	if nilInterface(request.Model) {
		return Result{}, codedError(CodeInvalidArgument, "probe tool calling", ErrInvalidArgument, errors.New("model is required"))
	}

	controls := executionControls{
		maxOutputTokens:         request.MaxOutputTokens,
		maxReturnedTextBytes:    probeMaxReturnedTextBytes,
		maxModelCalls:           1,
		maxToolCallsPerResponse: 1,
		toolExecution:           ToolExecutionSequential,
	}
	if validationErr := validateControls(controls); validationErr != nil {
		return Result{}, validationErr
	}
	if cause := ctx.Err(); cause != nil {
		return Result{}, codedError(CodeCanceled, "probe tool calling", ErrCanceled, cause)
	}
	statistics := &runStatistics{}
	failures := &failureRecorder{}
	responses := &responseRecorder{}
	protected, protectErr := protectModel(request.Model, statistics, failures, responses, controls)
	if protectErr != nil {
		return Result{}, protectErr
	}

	var modelResponse *model.LLMResponse
	multipleResponses := false
	for response, modelErr := range protected.GenerateContent(ctx, toolCallingProbeRequest(), false) {
		result.Metadata = statistics.metadata()
		result.Text = responses.textValue()
		if modelErr != nil {
			if failure := failures.failure(); failure != nil {
				return result, failure
			}
			return result, executionError(ctx, "probe tool calling", modelErr)
		}
		if modelResponse != nil {
			multipleResponses = true
			break
		}
		modelResponse = response
	}
	result.Metadata = statistics.metadata()
	result.Text = responses.textValue()
	if failure := failures.failure(); failure != nil {
		return result, failure
	}
	if modelResponse == nil {
		return result, codedError(CodeNoFinalResponse, "probe tool calling", ErrNoFinalResponse, nil)
	}
	if multipleResponses {
		return result, codedError(CodeToolCallingUnsupported, "probe tool calling", ErrToolCallingUnsupported, nil)
	}

	response := modelResponse
	if response.ErrorCode != "" || response.ErrorMessage != "" || response.Interrupted || response.Partial {
		return result, codedError(CodeExecutionFailed, "probe tool calling", ErrExecutionFailed, nil)
	}
	if response.FinishReason != genai.FinishReasonStop {
		return result, codedError(CodeExecutionFailed, "probe tool calling", ErrExecutionFailed, nil)
	}
	if response.Content == nil {
		return result, codedError(CodeNoFinalResponse, "probe tool calling", ErrNoFinalResponse, nil)
	}
	actionable := probeActionableParts(response.Content.Parts)
	if len(actionable) == 0 {
		return result, codedError(CodeNoFinalResponse, "probe tool calling", ErrNoFinalResponse, nil)
	}
	if response.Content.Role != genai.RoleModel {
		return result, codedError(CodeToolCallingUnsupported, "probe tool calling", ErrToolCallingUnsupported, nil)
	}
	if len(actionable) != 1 || !validProbeCall(actionable[0]) {
		return result, codedError(CodeToolCallingUnsupported, "probe tool calling", ErrToolCallingUnsupported, nil)
	}
	return result, nil
}

func toolCallingProbeRequest() *model.LLMRequest {
	return &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText(
			`Call plasmid_ping exactly once with {"marker":"plasmid-probe-v1"}. Return no text.`,
			genai.RoleUser,
		)},
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name:        probeToolName,
				Description: "Confirm function-calling support.",
				ParametersJsonSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						probeMarkerName: map[string]any{"type": "string", "enum": []string{probeMarkerValue}},
					},
					"required":             []string{probeMarkerName},
					"additionalProperties": false,
				},
			}}}},
			ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode:                 genai.FunctionCallingConfigModeAny,
				AllowedFunctionNames: []string{probeToolName},
			}},
		},
	}
}

func probeActionableParts(parts []*genai.Part) []*genai.Part {
	result := make([]*genai.Part, 0, len(parts))
	for _, part := range parts {
		if probeThoughtOnlyPart(part) {
			continue
		}
		result = append(result, part)
	}
	return result
}

func probeThoughtOnlyPart(part *genai.Part) bool {
	if part == nil || !part.Thought {
		return false
	}
	partCopy := *part
	partCopy.Text = ""
	partCopy.Thought = false
	partCopy.ThoughtSignature = nil
	return reflect.DeepEqual(partCopy, genai.Part{})
}

func validProbeCall(part *genai.Part) bool {
	if part == nil || part.FunctionCall == nil {
		return false
	}
	partCopy := *part
	call := partCopy.FunctionCall
	partCopy.FunctionCall = nil
	partCopy.ThoughtSignature = nil
	if !reflect.DeepEqual(partCopy, genai.Part{}) {
		return false
	}
	callCopy := *call
	callCopy.ID = ""
	callCopy.Name = ""
	callCopy.Args = nil
	marker, markerOK := call.Args[probeMarkerName].(string)
	return call.Name == probeToolName && len(call.Args) == 1 && markerOK && marker == probeMarkerValue &&
		reflect.DeepEqual(callCopy, genai.FunctionCall{})
}
