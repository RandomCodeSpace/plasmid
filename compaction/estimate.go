// Package compaction provides deterministic native Google ADK request
// estimation and durable context compaction callbacks.
package compaction

import (
	"bytes"
	"encoding/json"
	"errors"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const (
	// BytesPerToken is the fixture-pinned raw UTF-8 estimator ratio.
	BytesPerToken = 4

	// Structural overheads are tokens added after the raw byte estimate. They
	// model native request framing without pretending to be a provider tokenizer.
	ContentOverheadTokens         = 4
	PartOverheadTokens            = 1
	FunctionOverheadTokens        = 8
	BinaryOverheadTokens          = 16
	ToolDeclarationOverheadTokens = 12

	// ElisionMarker is the stable replacement for an eligible tool-response body.
	ElisionMarker = "[elided]"
)

// Estimate is the deterministic breakdown for one native model request.
type Estimate struct {
	CanonicalBytes   int `json:"canonicalBytes"`
	RawTokens        int `json:"rawTokens"`
	Contents         int `json:"contents"`
	Parts            int `json:"parts"`
	Functions        int `json:"functions"`
	Binaries         int `json:"binaries"`
	ToolDeclarations int `json:"toolDeclarations"`
	OverheadTokens   int `json:"overheadTokens"`
	Tokens           int `json:"tokens"`
}

type requestWire struct {
	Model    string                       `json:"model,omitempty"`
	Contents []*genai.Content             `json:"contents,omitempty"`
	Config   *genai.GenerateContentConfig `json:"config,omitempty"`
}

// EstimateRequest estimates the complete model-facing request. LLMRequest.Tools
// is intentionally excluded because ADK marks it as runtime-only and projects
// its declarations into Config.Tools before the model call.
func EstimateRequest(request *model.LLMRequest) (Estimate, error) {
	if request == nil {
		return Estimate{}, errors.New("estimate request: nil request")
	}
	canonical, err := canonicalJSON(requestWire{Model: request.Model, Contents: request.Contents, Config: request.Config})
	if err != nil {
		return Estimate{}, err
	}
	result := Estimate{CanonicalBytes: len(canonical)}
	result.RawTokens = ceilDiv(result.CanonicalBytes, BytesPerToken)
	for _, content := range request.Contents {
		countContent(content, &result)
	}
	if request.Config != nil {
		countContent(request.Config.SystemInstruction, &result)
		for _, configuredTool := range request.Config.Tools {
			if configuredTool == nil {
				continue
			}
			result.ToolDeclarations += len(configuredTool.FunctionDeclarations)
		}
	}
	result.OverheadTokens = result.Contents*ContentOverheadTokens +
		result.Parts*PartOverheadTokens +
		result.Functions*FunctionOverheadTokens +
		result.Binaries*BinaryOverheadTokens +
		result.ToolDeclarations*ToolDeclarationOverheadTokens
	result.Tokens = result.RawTokens + result.OverheadTokens
	return result, nil
}

func countContent(content *genai.Content, result *Estimate) {
	if content == nil {
		return
	}
	result.Contents++
	for _, part := range content.Parts {
		countPart(part, result)
	}
}

func countPart(part *genai.Part, result *Estimate) {
	if part == nil {
		return
	}
	result.Parts++
	if part.FunctionCall != nil {
		result.Functions++
	}
	if part.FunctionResponse != nil {
		result.Functions++
		countResponseBinaries(part.FunctionResponse.Parts, result)
	}
	if part.ToolCall != nil {
		result.Functions++
	}
	if part.ToolResponse != nil {
		result.Functions++
	}
	if part.InlineData != nil || len(part.ThoughtSignature) != 0 {
		result.Binaries++
	}
}

func countResponseBinaries(parts []*genai.FunctionResponsePart, result *Estimate) {
	for _, part := range parts {
		if part != nil && part.InlineData != nil {
			result.Binaries++
		}
	}
}

func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalized); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func ceilDiv(value, divisor int) int {
	if value <= 0 {
		return 0
	}
	return 1 + (value-1)/divisor
}
