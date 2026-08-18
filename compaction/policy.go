package compaction

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strconv"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/config"
)

const sidecarVersion = 1

type durableState struct {
	Version         int      `json:"version"`
	ElidedResponses []string `json:"elidedResponses,omitempty"`
	DroppedTurns    []string `json:"droppedTurns,omitempty"`
	Calibration     float64  `json:"calibration"`
	ExhaustedWarned bool     `json:"exhaustedWarned,omitempty"`
}

type policyResult struct {
	Estimate     Estimate
	Triggered    bool
	Exhausted    bool
	StateChanged bool
}

func applyPolicy(policy config.Compaction, state *durableState, request *model.LLMRequest) (policyResult, error) {
	normalizeDurableState(state)
	preserved := preservedToolNames(policy.PreserveToolNames)
	estimate, err := EstimateRequest(request)
	if err != nil {
		return policyResult{}, err
	}
	current := policyResult{Estimate: estimate}
	factor := calibrationFactor(policy.Calibration, state.Calibration)
	trigger := int(math.Floor(float64(policy.ContextTokens) * policy.TriggerFraction))
	sticky := len(state.DroppedTurns) > 0 || len(state.ElidedResponses) > 0
	if !sticky && calibratedTokens(estimate.Tokens, factor) < trigger {
		return current, nil
	}
	working := *request
	working.Contents = cloneContents(request.Contents)
	requestChanged, err := applyStickyState(policy, state, &working, &current, preserved, sticky)
	if err != nil {
		return policyResult{}, err
	}
	if calibratedTokens(current.Estimate.Tokens, factor) < trigger {
		if requestChanged {
			request.Contents = working.Contents
		}
		return current, nil
	}
	current.Triggered = true
	target := int(math.Floor(float64(policy.ContextTokens) * policy.TargetFraction))
	changed, err := elideResponses(policy, state, &working, &current, factor, target, preserved)
	if err != nil {
		return policyResult{}, err
	}
	requestChanged = requestChanged || changed
	changed, err = dropCompleteTurns(policy, state, &working, &current, factor, target, preserved)
	if err != nil {
		return policyResult{}, err
	}
	requestChanged = requestChanged || changed
	sort.Strings(state.ElidedResponses)
	sort.Strings(state.DroppedTurns)
	current.Exhausted = calibratedTokens(current.Estimate.Tokens, factor) > target
	if requestChanged {
		request.Contents = working.Contents
	}
	return current, nil
}

func calibrationFactor(enabled bool, stored float64) float64 {
	if enabled {
		return stored
	}
	return 1
}

func applyStickyState(policy config.Compaction, state *durableState, working *model.LLMRequest, current *policyResult, preserved map[string]struct{}, sticky bool) (bool, error) {
	if !sticky {
		return false, nil
	}
	dropped, err := applyStickyDrops(working, state.DroppedTurns, policy.KeepRecentContents, preserved)
	if err != nil {
		return false, err
	}
	elided, err := applyStickyElisions(working, state.ElidedResponses, policy.KeepRecentContents, preserved)
	if err != nil {
		return false, err
	}
	current.Estimate, err = EstimateRequest(working)
	return dropped || elided, err
}

func normalizeDurableState(state *durableState) {
	if state.Version == 0 {
		state.Version = sidecarVersion
	}
	if state.Calibration <= 0 || math.IsNaN(state.Calibration) || math.IsInf(state.Calibration, 0) {
		state.Calibration = 1
	}
}

func preservedToolNames(names []string) map[string]struct{} {
	preserved := make(map[string]struct{}, len(names))
	for _, name := range names {
		preserved[name] = struct{}{}
	}
	return preserved
}

func elideResponses(policy config.Compaction, state *durableState, working *model.LLMRequest, current *policyResult, factor float64, target int, preserved map[string]struct{}) (bool, error) {
	candidates, err := eligibleResponseCandidates(working, policy.KeepRecentContents)
	if err != nil {
		return false, err
	}
	changed := false
	for _, candidate := range candidates {
		if calibratedTokens(current.Estimate.Tokens, factor) <= target {
			break
		}
		if _, keep := preserved[candidate.name]; keep || candidate.bodyTokens < policy.MinimumElisionTokens {
			continue
		}
		candidate.elide()
		nextEstimate, estimateErr := EstimateRequest(working)
		if estimateErr != nil {
			candidate.restore()
			return false, estimateErr
		}
		if nextEstimate.Tokens >= current.Estimate.Tokens {
			candidate.restore()
			continue
		}
		state.ElidedResponses = append(state.ElidedResponses, candidate.key)
		current.StateChanged = true
		current.Estimate = nextEstimate
		changed = true
	}
	return changed, nil
}

func dropCompleteTurns(policy config.Compaction, state *durableState, working *model.LLMRequest, current *policyResult, factor float64, target int, preserved map[string]struct{}) (bool, error) {
	changed := false
	for calibratedTokens(current.Estimate.Tokens, factor) > target {
		groups, err := completeTurns(working.Contents)
		if err != nil {
			return false, err
		}
		candidate := firstDroppableTurn(working.Contents, groups, policy.KeepRecentContents, preserved)
		if candidate == nil {
			break
		}
		working.Contents = append(working.Contents[:candidate.start], working.Contents[candidate.end:]...)
		nextEstimate, err := EstimateRequest(working)
		if err != nil {
			return false, err
		}
		state.DroppedTurns = append(state.DroppedTurns, candidate.key)
		current.StateChanged = true
		current.Estimate = nextEstimate
		changed = true
	}
	return changed, nil
}

func calibratedTokens(raw int, factor float64) int {
	return int(math.Ceil(float64(raw) * factor))
}

func cloneContents(contents []*genai.Content) []*genai.Content {
	result := make([]*genai.Content, len(contents))
	for contentIndex, content := range contents {
		if content == nil {
			continue
		}
		contentCopy := *content
		contentCopy.Parts = make([]*genai.Part, len(content.Parts))
		for partIndex, part := range content.Parts {
			if part == nil {
				continue
			}
			partCopy := *part
			if part.FunctionResponse != nil {
				responseCopy := *part.FunctionResponse
				partCopy.FunctionResponse = &responseCopy
			}
			if part.ToolResponse != nil {
				responseCopy := *part.ToolResponse
				partCopy.ToolResponse = &responseCopy
			}
			contentCopy.Parts[partIndex] = &partCopy
		}
		result[contentIndex] = &contentCopy
	}
	return result
}

type responseCandidate struct {
	key        string
	name       string
	bodyTokens int
	elide      func()
	restore    func()
}

func eligibleResponseCandidates(request *model.LLMRequest, keepRecent int) ([]responseCandidate, error) {
	end := len(request.Contents) - keepRecent
	if end < 1 {
		end = 1
	}
	return responseCandidatesInRange(request, 1, end)
}

func responseCandidatesInRange(request *model.LLMRequest, start, end int) ([]responseCandidate, error) {
	var candidates []responseCandidate
	start, end = boundedRange(start, end, len(request.Contents))
	for _, content := range request.Contents[start:end] {
		if content == nil {
			continue
		}
		values, err := contentResponseCandidates(content)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, values...)
	}
	return candidates, nil
}

func boundedRange(start, end, length int) (int, int) {
	start = max(0, min(start, length))
	end = max(start, min(end, length))
	return start, end
}

func contentResponseCandidates(content *genai.Content) ([]responseCandidate, error) {
	var candidates []responseCandidate
	for _, part := range content.Parts {
		if part == nil {
			continue
		}
		candidate, ok, err := functionResponseCandidate(part.FunctionResponse)
		if err != nil {
			return nil, err
		}
		if ok {
			candidates = append(candidates, candidate)
		}
		candidate, ok, err = toolResponseCandidate(part.ToolResponse)
		if err != nil {
			return nil, err
		}
		if ok {
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

func functionResponseCandidate(response *genai.FunctionResponse) (responseCandidate, bool, error) {
	if response == nil || functionResponseElided(response) {
		return responseCandidate{}, false, nil
	}
	body := struct {
		Response map[string]any                `json:"response,omitempty"`
		Parts    []*genai.FunctionResponsePart `json:"parts,omitempty"`
	}{Response: response.Response, Parts: response.Parts}
	data, err := canonicalJSON(body)
	if err != nil {
		return responseCandidate{}, false, err
	}
	key, err := responseKey(response.ID, response.Name, body)
	if err != nil {
		return responseCandidate{}, false, err
	}
	originalResponse, originalParts := response.Response, response.Parts
	return responseCandidate{
		key: key, name: response.Name, bodyTokens: ceilDiv(len(data), BytesPerToken),
		elide: func() {
			response.Response = map[string]any{"output": ElisionMarker}
			response.Parts = nil
		},
		restore: func() {
			response.Response = originalResponse
			response.Parts = originalParts
		},
	}, true, nil
}

func toolResponseCandidate(response *genai.ToolResponse) (responseCandidate, bool, error) {
	if response == nil || toolResponseElided(response) {
		return responseCandidate{}, false, nil
	}
	data, err := canonicalJSON(response.Response)
	if err != nil {
		return responseCandidate{}, false, err
	}
	key, err := responseKey(response.ID, string(response.ToolType), response.Response)
	if err != nil {
		return responseCandidate{}, false, err
	}
	originalResponse := response.Response
	return responseCandidate{
		key: key, name: string(response.ToolType), bodyTokens: ceilDiv(len(data), BytesPerToken),
		elide:   func() { response.Response = map[string]any{"output": ElisionMarker} },
		restore: func() { response.Response = originalResponse },
	}, true, nil
}

func responseKey(id, name string, body any) (string, error) {
	if id != "" {
		return "id:" + id, nil
	}
	data, err := canonicalJSON(struct {
		Name string `json:"name"`
		Body any    `json:"body"`
	}{Name: name, Body: body})
	if err != nil {
		return "", err
	}
	return "sha256:" + digest(data), nil
}

func functionResponseElided(response *genai.FunctionResponse) bool {
	if len(response.Parts) != 0 || len(response.Response) != 1 {
		return false
	}
	value, ok := response.Response["output"].(string)
	return ok && value == ElisionMarker
}

func toolResponseElided(response *genai.ToolResponse) bool {
	if len(response.Response) != 1 {
		return false
	}
	value, ok := response.Response["output"].(string)
	return ok && value == ElisionMarker
}

func applyStickyElisions(request *model.LLMRequest, values []string, keepRecent int, preserved map[string]struct{}) (bool, error) {
	remaining := stringCounts(values)
	candidates, err := eligibleResponseCandidates(request, keepRecent)
	if err != nil {
		return false, err
	}
	current, err := EstimateRequest(request)
	if err != nil {
		return false, err
	}
	changed := false
	for _, candidate := range candidates {
		if _, keep := preserved[candidate.name]; keep {
			continue
		}
		if remaining[candidate.key] == 0 {
			continue
		}
		candidate.elide()
		next, err := EstimateRequest(request)
		if err != nil {
			candidate.restore()
			return false, err
		}
		if next.Tokens >= current.Tokens {
			candidate.restore()
			continue
		}
		current = next
		remaining[candidate.key]--
		changed = true
	}
	return changed, nil
}

type turn struct {
	start int
	end   int
	key   string
}

func completeTurns(contents []*genai.Content) ([]turn, error) {
	var starts []int
	for index, content := range contents {
		if startsPromptTurn(content) {
			starts = append(starts, index)
		}
	}
	var result []turn
	for index := 0; index+1 < len(starts); index++ {
		start, end := starts[index], starts[index+1]
		key, err := turnKey(contents[start:end])
		if err != nil {
			return nil, err
		}
		result = append(result, turn{start: start, end: end, key: key})
	}
	return result, nil
}

func startsPromptTurn(content *genai.Content) bool {
	if content == nil || content.Role != string(genai.RoleUser) {
		return false
	}
	for _, part := range content.Parts {
		if part != nil && (part.FunctionResponse != nil || part.ToolResponse != nil) {
			return false
		}
	}
	return true
}

func turnKey(contents []*genai.Content) (string, error) {
	clone := cloneContents(contents)
	for _, content := range clone {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			if part.FunctionResponse != nil {
				part.FunctionResponse.Response = nil
				part.FunctionResponse.Parts = nil
			}
			if part.ToolResponse != nil {
				part.ToolResponse.Response = nil
			}
		}
	}
	data, err := canonicalJSON(clone)
	if err != nil {
		return "", err
	}
	return "sha256:" + digest(data), nil
}

func applyStickyDrops(request *model.LLMRequest, values []string, keepRecent int, preserved map[string]struct{}) (bool, error) {
	remaining := stringCounts(values)
	changed := false
	for len(remaining) > 0 {
		groups, err := completeTurns(request.Contents)
		if err != nil {
			return false, err
		}
		candidate := firstDroppableTurn(request.Contents, groups, keepRecent, preserved)
		for candidate != nil {
			if remaining[candidate.key] > 0 {
				break
			}
			candidate = nextDroppableTurn(request.Contents, groups, keepRecent, candidate.end, preserved)
		}
		if candidate == nil {
			return changed, nil
		}
		request.Contents = append(request.Contents[:candidate.start], request.Contents[candidate.end:]...)
		remaining[candidate.key]--
		if remaining[candidate.key] == 0 {
			delete(remaining, candidate.key)
		}
		changed = true
	}
	return changed, nil
}

func firstDroppableTurn(contents []*genai.Content, groups []turn, keepRecent int, preserved map[string]struct{}) *turn {
	return nextDroppableTurn(contents, groups, keepRecent, 0, preserved)
}

func nextDroppableTurn(contents []*genai.Content, groups []turn, keepRecent, after int, preserved map[string]struct{}) *turn {
	cutoff := len(contents) - keepRecent
	if cutoff < 0 {
		cutoff = 0
	}
	for index := range groups {
		group := &groups[index]
		if group.start < after || group.start == 0 || group.end > cutoff {
			continue
		}
		if turnContainsPreservedTool(contents[group.start:group.end], preserved) {
			continue
		}
		if pairsContained(contents, group.start, group.end) {
			return group
		}
	}
	return nil
}

func turnContainsPreservedTool(contents []*genai.Content, preserved map[string]struct{}) bool {
	for _, content := range contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if partContainsPreservedTool(part, preserved) {
				return true
			}
		}
	}
	return false
}

func partContainsPreservedTool(part *genai.Part, preserved map[string]struct{}) bool {
	if part == nil {
		return false
	}
	if part.FunctionCall != nil {
		_, ok := preserved[part.FunctionCall.Name]
		return ok
	}
	if part.FunctionResponse != nil {
		_, ok := preserved[part.FunctionResponse.Name]
		return ok
	}
	if part.ToolCall != nil {
		_, ok := preserved[string(part.ToolCall.ToolType)]
		return ok
	}
	if part.ToolResponse != nil {
		_, ok := preserved[string(part.ToolResponse.ToolType)]
		return ok
	}
	return false
}

func pairsContained(contents []*genai.Content, start, end int) bool {
	all := collectPairLocations(contents)
	for _, pair := range all {
		if !pairContainedInRange(pair, start, end) {
			return false
		}
	}
	return true
}

func collectPairLocations(contents []*genai.Content) map[string]*pairLocations {
	all := make(map[string]*pairLocations)
	callOccurrences := make(map[string]int)
	responseOccurrences := make(map[string]int)
	for contentIndex, content := range contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			recordPartLocations(all, part, contentIndex, callOccurrences, responseOccurrences)
		}
	}
	return all
}

func recordPartLocations(all map[string]*pairLocations, part *genai.Part, contentIndex int, callOccurrences, responseOccurrences map[string]int) {
	if part == nil {
		return
	}
	if call := part.FunctionCall; call != nil {
		recordPairLocation(all, occurrencePairKey(call.ID, "function:"+call.Name, callOccurrences), contentIndex, pairCall)
	}
	if response := part.FunctionResponse; response != nil {
		recordPairLocation(all, occurrencePairKey(response.ID, "function:"+response.Name, responseOccurrences), contentIndex, pairResponse)
	}
	if call := part.ToolCall; call != nil {
		recordPairLocation(all, occurrencePairKey(call.ID, "tool:"+string(call.ToolType), callOccurrences), contentIndex, pairCall)
	}
	if response := part.ToolResponse; response != nil {
		recordPairLocation(all, occurrencePairKey(response.ID, "tool:"+string(response.ToolType), responseOccurrences), contentIndex, pairResponse)
	}
}

func pairContainedInRange(pair *pairLocations, start, end int) bool {
	inside := anyInside(pair.calls, start, end) || anyInside(pair.responses, start, end)
	if !inside {
		return true
	}
	return len(pair.calls) == len(pair.responses) && allInside(pair.calls, start, end) && allInside(pair.responses, start, end)
}

type pairLocations struct {
	calls     []int
	responses []int
}

type pairSide uint8

const (
	pairCall pairSide = iota
	pairResponse
)

func recordPairLocation(all map[string]*pairLocations, key string, contentIndex int, side pairSide) {
	locations := all[key]
	if locations == nil {
		locations = &pairLocations{}
		all[key] = locations
	}
	if side == pairCall {
		locations.calls = append(locations.calls, contentIndex)
		return
	}
	locations.responses = append(locations.responses, contentIndex)
}

func occurrencePairKey(id, qualifiedName string, occurrences map[string]int) string {
	if id != "" {
		return "id:" + id
	}
	occurrence := occurrences[qualifiedName]
	occurrences[qualifiedName]++
	return "name:" + qualifiedName + "#" + strconv.Itoa(occurrence)
}

func anyInside(values []int, start, end int) bool {
	for _, value := range values {
		if value >= start && value < end {
			return true
		}
	}
	return false
}

func allInside(values []int, start, end int) bool {
	for _, value := range values {
		if value < start || value >= end {
			return false
		}
	}
	return true
}

func stringCounts(values []string) map[string]int {
	result := make(map[string]int, len(values))
	for _, value := range values {
		result[value]++
	}
	return result
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
