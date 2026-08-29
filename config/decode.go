package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RandomCodeSpace/plasmid/warning"
)

const (
	repaired          = " repaired"
	repairedToDefault = " repaired to default"
)

func decodeTop(operation *loadOperation, object map[string]json.RawMessage, configuration *Config, fallback Config, baseDir, homeDir string, warnings *warningCollector) error {
	warnUnknown(operation, object, []string{"appName", "compaction", "context", "foreign", "lsp", "mcp", "skills", "syntax", "tools", "version"}, "", warnings)
	version := 0
	if raw, ok := object["version"]; ok {
		if isJSONNull(raw) {
			return fmt.Errorf("version must be an integer: %w", ErrInvalidConfig)
		}
		if err := json.Unmarshal(raw, &version); err != nil {
			return fmt.Errorf("version must be an integer: %w", ErrInvalidConfig)
		}
	}
	switch {
	case version == 0:
		warnings.add(warning.WarnConfigVersionDefaulted, "version zero upgraded to version 1")
		configuration.Version = CurrentVersion
	case version < 0:
		return fmt.Errorf("version %d: %w", version, ErrInvalidConfig)
	case version > CurrentVersion:
		return fmt.Errorf("version %d: %w", version, ErrUnsupportedVersion)
	default:
		configuration.Version = version
	}
	decodeNonEmptyString(object, "appName", &configuration.AppName, "appName", warnings)
	if err := decodeLSP(operation, object["lsp"], &configuration.LSP, baseDir, homeDir, warnings); err != nil {
		return err
	}
	if err := decodeMCP(operation, object["mcp"], &configuration.MCP, baseDir, homeDir, warnings); err != nil {
		return err
	}
	paths := pathDecodeContext{baseDir: baseDir, homeDir: homeDir, warnings: warnings}
	if err := decodePathBlock(operation, object["skills"], "skills", "roots", &configuration.Skills.Roots, paths); err != nil {
		return err
	}
	if err := decodeForeign(operation, object["foreign"], &configuration.Foreign, baseDir, homeDir, warnings); err != nil {
		return err
	}
	decodeSyntax(operation, object["syntax"], &configuration.Syntax, warnings)
	if err := decodeContext(operation, object["context"], &configuration.Context, baseDir, homeDir, warnings); err != nil {
		return err
	}
	decodeTools(operation, object["tools"], &configuration.Tools, fallback.Tools, warnings)
	decodeCompaction(operation, object["compaction"], &configuration.Compaction, fallback.Compaction, warnings)
	return nil
}

func decodeLSP(operation *loadOperation, raw json.RawMessage, value *LSP, baseDir, homeDir string, warnings *warningCollector) error {
	if err := operation.check(); err != nil {
		return err
	}
	object, ok := optionalObject(raw, "lsp", warnings)
	if !ok {
		return nil
	}
	warnUnknown(operation, object, []string{"failureThreshold", "initializeTimeoutMs", "maxDiagnosticsPerFile", "mode", "requestTimeoutMs", "servers", "settleTimeoutMs"}, "lsp", warnings)
	if mode, present := optionalString(object, "mode", "lsp.mode", warnings); present {
		switch LSPMode(mode) {
		case LSPAuto, LSPOff:
			value.Mode = LSPMode(mode)
		default:
			warnings.add(warning.WarnConfigBadEnum, "lsp.mode repaired to default")
		}
	}
	decodePositiveDuration(object, "settleTimeoutMs", &value.SettleTimeout, "lsp.settleTimeoutMs", warnings)
	decodePositiveDuration(object, "initializeTimeoutMs", &value.InitializeTimeout, "lsp.initializeTimeoutMs", warnings)
	decodePositiveDuration(object, "requestTimeoutMs", &value.RequestTimeout, "lsp.requestTimeoutMs", warnings)
	decodePositiveInt(object, "failureThreshold", &value.FailureThreshold, "lsp.failureThreshold", warnings)
	decodePositiveInt(object, "maxDiagnosticsPerFile", &value.MaxDiagnosticsPerFile, "lsp.maxDiagnosticsPerFile", warnings)
	serversRaw, present := object["servers"]
	if !present {
		return nil
	}
	var entries []json.RawMessage
	if isJSONNull(serversRaw) || json.Unmarshal(serversRaw, &entries) != nil {
		warnings.add(warning.WarnConfigInvalidValue, "lsp.servers repaired to defaults")
		return nil
	}
	return decodeLSPServers(operation, entries, value, baseDir, homeDir, warnings)
}

func decodeLSPServers(operation *loadOperation, entries []json.RawMessage, value *LSP, baseDir, homeDir string, warnings *warningCollector) error {
	decoder := lspServerDecoder{
		operation: operation, value: value, baseDir: baseDir, homeDir: homeDir, warnings: warnings,
		positions: make(map[string]int, len(value.Servers)), seen: make(map[string]struct{}, len(entries)),
	}
	for index := range value.Servers {
		decoder.positions[value.Servers[index].ID] = index
	}
	for index, entryRaw := range entries {
		if err := operation.check(); err != nil {
			return err
		}
		server, entry, position, existing, valid, err := decoder.decode(entryRaw, index)
		if err != nil {
			return err
		}
		if !valid {
			continue
		}
		warnUnknown(operation, entry, []string{"args", "command", "disabled", "extensions", "id", "rootMarkers"}, fmt.Sprintf("lsp.servers[%d]", index), warnings)
		if err := operation.check(); err != nil {
			return err
		}
		if existing {
			value.Servers[position] = server
		} else {
			decoder.positions[server.ID] = len(value.Servers)
			value.Servers = append(value.Servers, server)
		}
		decoder.seen[server.ID] = struct{}{}
	}
	return nil
}

type lspServerDecoder struct {
	operation        *loadOperation
	value            *LSP
	baseDir, homeDir string
	warnings         *warningCollector
	positions        map[string]int
	seen             map[string]struct{}
}

func (d lspServerDecoder) decode(raw json.RawMessage, index int) (LSPServer, map[string]json.RawMessage, int, bool, bool, error) {
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entry); err != nil || entry == nil {
		d.warnings.add(warning.WarnLSPConfigInvalidServer, fmt.Sprintf("lsp.servers[%d] dropped", index))
		return LSPServer{}, nil, 0, false, false, nil
	}
	id, valid := requiredString(entry, "id")
	if !valid {
		d.warnings.add(warning.WarnLSPConfigInvalidServer, fmt.Sprintf("lsp.servers[%d] dropped", index))
		return LSPServer{}, nil, 0, false, false, nil
	}
	if _, duplicate := d.seen[id]; duplicate {
		d.warnings.add(warning.WarnLSPConfigDuplicateServer, fmt.Sprintf("duplicate lsp.servers[%d] dropped", index))
		return LSPServer{}, nil, 0, false, false, nil
	}
	position, existing := d.positions[id]
	server := LSPServer{ID: id}
	if existing {
		server = cloneLSP(LSP{Servers: []LSPServer{d.value.Servers[position]}}).Servers[0]
	}
	valid, err := decodeLSPServerFields(d.operation, entry, &server, d.baseDir, d.homeDir)
	if !valid && err == nil {
		d.warnings.add(warning.WarnLSPConfigInvalidServer, fmt.Sprintf("lsp.servers[%d] dropped", index))
	}
	return server, entry, position, existing, valid, err
}

func decodeLSPServerFields(operation *loadOperation, entry map[string]json.RawMessage, server *LSPServer, baseDir, homeDir string) (bool, error) {
	valid, err := decodeLSPCommand(operation, entry["command"], server, baseDir, homeDir)
	if err != nil || !valid {
		return valid, err
	}
	for _, field := range []struct {
		name        string
		destination *[]string
	}{
		{name: "args", destination: &server.Args},
		{name: "extensions", destination: &server.Extensions},
		{name: "rootMarkers", destination: &server.RootMarkers},
	} {
		valid, err = decodeLSPStringSlice(operation, entry, field.name, field.destination)
		if err != nil || !valid {
			return valid, err
		}
	}
	if raw, present := entry["disabled"]; present {
		server.Disabled, valid = strictBool(raw)
		if !valid {
			return false, nil
		}
	}
	if server.Command == "" || len(server.Extensions) == 0 {
		return false, nil
	}
	return validLSPExtensions(operation, server.Extensions)
}

func decodeLSPCommand(operation *loadOperation, raw json.RawMessage, server *LSPServer, baseDir, homeDir string) (bool, error) {
	if raw == nil {
		return true, nil
	}
	command, valid := strictString(raw)
	command = strings.TrimSpace(command)
	if !valid || command == "" {
		return false, nil
	}
	resolved, err := normalizeCommand(operation, command, baseDir, homeDir)
	if err != nil {
		if contextErr := operation.check(); contextErr != nil {
			return false, contextErr
		}
		return false, nil
	}
	server.Command = resolved
	return true, nil
}

func decodeLSPStringSlice(operation *loadOperation, entry map[string]json.RawMessage, key string, destination *[]string) (bool, error) {
	raw, present := entry[key]
	if !present {
		return true, nil
	}
	items, valid := strictStringSlice(operation, raw)
	if err := operation.check(); err != nil {
		return false, err
	}
	if valid {
		*destination = items
	}
	return valid, nil
}

func validLSPExtensions(operation *loadOperation, extensions []string) (bool, error) {
	for _, extension := range extensions {
		if err := operation.check(); err != nil {
			return false, err
		}
		if !strings.HasPrefix(extension, ".") || strings.ContainsAny(extension, `/\\`) {
			return false, nil
		}
	}
	return true, nil
}

func decodeMCP(operation *loadOperation, raw json.RawMessage, value *MCP, baseDir, homeDir string, warnings *warningCollector) error {
	if err := operation.check(); err != nil {
		return err
	}
	object, ok := optionalObject(raw, "mcp", warnings)
	if !ok {
		return nil
	}
	warnUnknown(operation, object, []string{"allowForeign", "inheritForeign", "servers"}, "mcp", warnings)
	decodeBool(object, "inheritForeign", &value.InheritForeign, "mcp.inheritForeign", warnings)
	decodeMCPAllowlist(operation, object, value, warnings)
	rawServers, present := object["servers"]
	if !present {
		return nil
	}
	return decodeMCPServers(operation, rawServers, value, baseDir, homeDir, warnings)
}

func decodeMCPAllowlist(operation *loadOperation, object map[string]json.RawMessage, value *MCP, warnings *warningCollector) {
	raw, present := object["allowForeign"]
	if !present {
		return
	}
	if entries, valid := decodeStringElements(operation, raw, "mcp.allowForeign", warnings); valid {
		value.AllowForeign = uniqueExactNames(operation, entries, warnings)
	}
}

func decodeMCPServers(operation *loadOperation, raw json.RawMessage, value *MCP, baseDir, homeDir string, warnings *warningCollector) error {
	var entries []json.RawMessage
	if isJSONNull(raw) || json.Unmarshal(raw, &entries) != nil {
		warnings.add(warning.WarnConfigInvalidValue, "mcp.servers repaired to empty")
		return nil
	}
	value.Servers = nil
	seen := make(map[string]struct{}, len(entries))
	for index, rawEntry := range entries {
		if err := operation.check(); err != nil {
			return err
		}
		server, entry, valid, err := decodeMCPServer(operation, rawEntry, index, baseDir, homeDir, warnings)
		if err != nil {
			return err
		}
		if !valid {
			continue
		}
		if _, duplicate := seen[server.ID]; duplicate {
			warnings.add(warning.WarnConfigMCPIncomplete, fmt.Sprintf("duplicate mcp.servers[%d] dropped", index))
			continue
		}
		warnUnknown(operation, entry, []string{"args", "command", "env", "headers", "id", "transport", "url"}, fmt.Sprintf("mcp.servers[%d]", index), warnings)
		if err := operation.check(); err != nil {
			return err
		}
		seen[server.ID] = struct{}{}
		value.Servers = append(value.Servers, server)
	}
	return nil
}

func decodeMCPServer(operation *loadOperation, raw json.RawMessage, index int, baseDir, homeDir string, warnings *warningCollector) (MCPServer, map[string]json.RawMessage, bool, error) {
	if err := operation.check(); err != nil {
		return MCPServer{}, nil, false, err
	}
	object, server, valid := decodeMCPServerHeader(raw)
	if !valid {
		return dropMCPServer(index, warnings)
	}
	if server.Transport == MCPStdio {
		return decodeStdioMCPEntry(operation, object, server, index, baseDir, homeDir, warnings)
	}
	return decodeHTTPMCPEntry(operation, object, server, index, warnings)
}

func decodeMCPServerHeader(raw json.RawMessage) (map[string]json.RawMessage, MCPServer, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, MCPServer{}, false
	}
	id, ok := requiredString(object, "id")
	if !ok {
		return nil, MCPServer{}, false
	}
	transport := MCPStdio
	if rawTransport, present := object["transport"]; present {
		text, valid := strictString(rawTransport)
		if !valid {
			return nil, MCPServer{}, false
		}
		transport = MCPTransport(text)
	}
	if transport != MCPStdio && transport != MCPHTTP {
		return nil, MCPServer{}, false
	}
	return object, MCPServer{ID: id, Transport: transport}, true
}

func decodeStdioMCPEntry(operation *loadOperation, object map[string]json.RawMessage, server MCPServer, index int, baseDir, homeDir string, warnings *warningCollector) (MCPServer, map[string]json.RawMessage, bool, error) {
	server, valid, err := decodeStdioMCPServer(operation, object, server, baseDir, homeDir)
	if err != nil {
		return MCPServer{}, nil, false, err
	}
	if !valid {
		return dropMCPServer(index, warnings)
	}
	return server, object, true, nil
}

func decodeHTTPMCPEntry(operation *loadOperation, object map[string]json.RawMessage, server MCPServer, index int, warnings *warningCollector) (MCPServer, map[string]json.RawMessage, bool, error) {
	server, valid, err := decodeHTTPMCPServer(operation, object, server)
	if err != nil {
		return MCPServer{}, nil, false, err
	}
	if !valid {
		return dropMCPServer(index, warnings)
	}
	return server, object, true, nil
}

func dropMCPServer(index int, warnings *warningCollector) (MCPServer, map[string]json.RawMessage, bool, error) {
	warnings.add(warning.WarnConfigMCPIncomplete, fmt.Sprintf("mcp.servers[%d] dropped", index))
	return MCPServer{}, nil, false, nil
}

func decodeStdioMCPServer(operation *loadOperation, object map[string]json.RawMessage, server MCPServer, baseDir, homeDir string) (MCPServer, bool, error) {
	if hasAnyField(object, "headers", "url") {
		return MCPServer{}, false, nil
	}
	command, valid := strictString(object["command"])
	command = strings.TrimSpace(command)
	if !valid || command == "" {
		return MCPServer{}, false, nil
	}
	args, valid, err := optionalStrictStringSlice(operation, object, "args")
	if err != nil || !valid {
		return MCPServer{}, valid, err
	}
	server.Args = args
	environment, valid, err := optionalStrictStringMap(operation, object, "env")
	if err != nil || !valid {
		return MCPServer{}, valid, err
	}
	server.Env = environment
	resolved, err := normalizeCommand(operation, command, baseDir, homeDir)
	if err != nil {
		if contextErr := operation.check(); contextErr != nil {
			return MCPServer{}, false, contextErr
		}
		return MCPServer{}, false, nil
	}
	server.Command = resolved
	return server, true, nil
}

func hasAnyField(object map[string]json.RawMessage, fields ...string) bool {
	for _, field := range fields {
		if _, present := object[field]; present {
			return true
		}
	}
	return false
}

func optionalStrictStringSlice(operation *loadOperation, object map[string]json.RawMessage, key string) ([]string, bool, error) {
	raw, present := object[key]
	if !present {
		return nil, true, nil
	}
	items, valid := strictStringSlice(operation, raw)
	if err := operation.check(); err != nil {
		return nil, false, err
	}
	return items, valid, nil
}

func optionalStrictStringMap(operation *loadOperation, object map[string]json.RawMessage, key string) (map[string]string, bool, error) {
	raw, present := object[key]
	if !present {
		return nil, true, nil
	}
	items, valid := strictStringMap(operation, raw)
	if err := operation.check(); err != nil {
		return nil, false, err
	}
	return items, valid, nil
}

func decodeHTTPMCPServer(operation *loadOperation, object map[string]json.RawMessage, server MCPServer) (MCPServer, bool, error) {
	for _, forbidden := range []string{"args", "command", "env"} {
		if _, present := object[forbidden]; present {
			return MCPServer{}, false, nil
		}
	}
	address, valid := strictString(object["url"])
	parsed, err := url.Parse(address)
	if !valid || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return MCPServer{}, false, nil
	}
	server.URL = address
	if rawHeaders, present := object["headers"]; present {
		items, valid := strictStringMap(operation, rawHeaders)
		if err := operation.check(); err != nil {
			return MCPServer{}, false, err
		}
		if !valid {
			return MCPServer{}, false, nil
		}
		server.Headers = items
	}
	return server, true, nil
}

type pathDecodeContext struct {
	baseDir, homeDir string
	warnings         *warningCollector
}

func decodePathBlock(operation *loadOperation, raw json.RawMessage, block, key string, destination *[]string, paths pathDecodeContext) error {
	if err := operation.check(); err != nil {
		return err
	}
	object, ok := optionalObject(raw, block, paths.warnings)
	if !ok {
		return nil
	}
	warnUnknown(operation, object, []string{key}, block, paths.warnings)
	return decodePaths(operation, object, key, destination, block+"."+key, paths)
}

func decodeForeign(operation *loadOperation, raw json.RawMessage, value *Foreign, baseDir, homeDir string, warnings *warningCollector) error {
	if err := operation.check(); err != nil {
		return err
	}
	object, ok := optionalObject(raw, "foreign", warnings)
	if !ok {
		return nil
	}
	warnUnknown(operation, object, []string{"claude", "codex", "copilot", "enabled", "trustedRoots"}, "foreign", warnings)
	decodeBool(object, "enabled", &value.Enabled, "foreign.enabled", warnings)
	decodeBool(object, "claude", &value.Claude, "foreign.claude", warnings)
	decodeBool(object, "codex", &value.Codex, "foreign.codex", warnings)
	decodeBool(object, "copilot", &value.Copilot, "foreign.copilot", warnings)
	return decodePaths(operation, object, "trustedRoots", &value.TrustedRoots, "foreign.trustedRoots", pathDecodeContext{baseDir: baseDir, homeDir: homeDir, warnings: warnings})
}

func decodeSyntax(operation *loadOperation, raw json.RawMessage, value *Syntax, warnings *warningCollector) {
	object, ok := optionalObject(raw, "syntax", warnings)
	if !ok {
		return
	}
	warnUnknown(operation, object, []string{"commandOutputBytes", "commandTimeoutMs", "documentOutputBytes", "documentTimeoutMs", "promptCommands"}, "syntax", warnings)
	if mode, present := optionalString(object, "promptCommands", "syntax.promptCommands", warnings); present {
		switch PromptCommandMode(mode) {
		case PromptCommandsOff, PromptCommandsTrusted, PromptCommandsOn:
			value.PromptCommands = PromptCommandMode(mode)
		default:
			warnings.add(warning.WarnConfigBadEnum, "syntax.promptCommands repaired to default")
		}
	}
	decodePositiveDuration(object, "commandTimeoutMs", &value.CommandTimeout, "syntax.commandTimeoutMs", warnings)
	decodePositiveDuration(object, "documentTimeoutMs", &value.DocumentTimeout, "syntax.documentTimeoutMs", warnings)
	decodePositiveInt(object, "commandOutputBytes", &value.CommandOutputBytes, "syntax.commandOutputBytes", warnings)
	decodePositiveInt(object, "documentOutputBytes", &value.DocumentOutputBytes, "syntax.documentOutputBytes", warnings)
}

func decodeContext(operation *loadOperation, raw json.RawMessage, value *Context, baseDir, homeDir string, warnings *warningCollector) error {
	if err := operation.check(); err != nil {
		return err
	}
	object, ok := optionalObject(raw, "context", warnings)
	if !ok {
		return nil
	}
	warnUnknown(operation, object, []string{"importRoots", "maxBytes", "maxFileBytes", "maxImportDepth", "touchesPerToolCall"}, "context", warnings)
	decodePositiveInt(object, "maxFileBytes", &value.MaxFileBytes, "context.maxFileBytes", warnings)
	decodePositiveInt(object, "maxBytes", &value.MaxBytes, "context.maxBytes", warnings)
	decodeNonNegativeInt(object, "maxImportDepth", &value.MaxImportDepth, "context.maxImportDepth", warnings)
	decodePositiveInt(object, "touchesPerToolCall", &value.TouchesPerToolCall, "context.touchesPerToolCall", warnings)
	return decodePaths(operation, object, "importRoots", &value.ImportRoots, "context.importRoots", pathDecodeContext{baseDir: baseDir, homeDir: homeDir, warnings: warnings})
}

func decodeTools(operation *loadOperation, raw json.RawMessage, value *Tools, fallback Tools, warnings *warningCollector) {
	object, ok := optionalObject(raw, "tools", warnings)
	if !ok {
		return
	}
	warnUnknown(operation, object, []string{"bashMaxTimeoutMs", "bashTimeoutMs", "callOutputBytes", "confirmation", "sessionOutputBytes"}, "tools", warnings)
	decodePositiveInt(object, "callOutputBytes", &value.CallOutputBytes, "tools.callOutputBytes", warnings)
	decodePositiveInt(object, "sessionOutputBytes", &value.SessionOutputBytes, "tools.sessionOutputBytes", warnings)
	decodePositiveDuration(object, "bashMaxTimeoutMs", &value.BashMaxTimeout, "tools.bashMaxTimeoutMs", warnings)
	decodePositiveDuration(object, "bashTimeoutMs", &value.BashTimeout, "tools.bashTimeoutMs", warnings)
	decodeBool(object, "confirmation", &value.Confirmation, "tools.confirmation", warnings)
	if value.BashTimeout > value.BashMaxTimeout {
		value.BashTimeout = fallback.BashTimeout
		value.BashMaxTimeout = fallback.BashMaxTimeout
		warnings.add(warning.WarnConfigInvalidValue, "tools.bashTimeoutMs repaired to default")
	}
}

func decodeCompaction(operation *loadOperation, raw json.RawMessage, value *Compaction, fallback Compaction, warnings *warningCollector) {
	object, ok := optionalObject(raw, "compaction", warnings)
	if !ok {
		return
	}
	warnUnknown(operation, object, []string{"calibration", "contextTokens", "keepRecentContents", "minimumElisionTokens", "preserveToolNames", "targetFraction", "triggerFraction"}, "compaction", warnings)
	decodeNonNegativeInt(object, "contextTokens", &value.ContextTokens, "compaction.contextTokens", warnings)
	decodeFraction(object, "triggerFraction", &value.TriggerFraction, "compaction.triggerFraction", warnings)
	decodeFraction(object, "targetFraction", &value.TargetFraction, "compaction.targetFraction", warnings)
	decodeNonNegativeInt(object, "keepRecentContents", &value.KeepRecentContents, "compaction.keepRecentContents", warnings)
	decodeNonNegativeInt(object, "minimumElisionTokens", &value.MinimumElisionTokens, "compaction.minimumElisionTokens", warnings)
	decodeStringSlice(operation, object, "preserveToolNames", &value.PreserveToolNames, "compaction.preserveToolNames", warnings)
	if operation.check() != nil {
		return
	}
	value.PreserveToolNames = uniqueStrings(operation, value.PreserveToolNames)
	decodeBool(object, "calibration", &value.Calibration, "compaction.calibration", warnings)
	if value.TargetFraction >= value.TriggerFraction {
		value.TriggerFraction = fallback.TriggerFraction
		value.TargetFraction = fallback.TargetFraction
		warnings.add(warning.WarnConfigInvalidValue, "compaction fractions repaired to defaults")
	}
}

func optionalObject(raw json.RawMessage, field string, warnings *warningCollector) (map[string]json.RawMessage, bool) {
	if raw == nil {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		warnings.add(warning.WarnConfigInvalidValue, field+" repaired to defaults")
		return nil, false
	}
	return object, true
}

func warnUnknown(operation *loadOperation, object map[string]json.RawMessage, known []string, scope string, warnings *warningCollector) {
	allowed := make(map[string]struct{}, len(known))
	for _, key := range known {
		allowed[key] = struct{}{}
	}
	var unknown []string
	for key := range object {
		if operation.check() != nil {
			return
		}
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		if operation.check() != nil {
			return
		}
		field := key
		if scope != "" {
			field = scope + "." + key
		}
		warnings.add(warning.WarnConfigUnknownKey, field+" ignored")
	}
}

func decodeNonEmptyString(object map[string]json.RawMessage, key string, destination *string, field string, warnings *warningCollector) {
	if value, present := optionalString(object, key, field, warnings); present {
		if strings.TrimSpace(value) == "" {
			warnings.add(warning.WarnConfigInvalidValue, field+repairedToDefault)
			return
		}
		*destination = value
	}
}

func optionalString(object map[string]json.RawMessage, key, field string, warnings *warningCollector) (string, bool) {
	raw, present := object[key]
	if !present {
		return "", false
	}
	var value string
	if isJSONNull(raw) || json.Unmarshal(raw, &value) != nil {
		warnings.add(warning.WarnConfigInvalidValue, field+repaired)
		return "", false
	}
	return value, true
}

func requiredString(object map[string]json.RawMessage, key string) (string, bool) {
	var value string
	raw, present := object[key]
	if !present || json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

func decodeBool(object map[string]json.RawMessage, key string, destination *bool, field string, warnings *warningCollector) {
	raw, present := object[key]
	if !present {
		return
	}
	var value bool
	if isJSONNull(raw) {
		warnings.add(warning.WarnConfigInvalidValue, field+repairedToDefault)
		return
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		warnings.add(warning.WarnConfigInvalidValue, field+repairedToDefault)
		return
	}
	*destination = value
}

func decodePositiveInt(object map[string]json.RawMessage, key string, destination *int, field string, warnings *warningCollector) {
	decodeInt(object, key, destination, field, false, warnings)
}

func decodeNonNegativeInt(object map[string]json.RawMessage, key string, destination *int, field string, warnings *warningCollector) {
	decodeInt(object, key, destination, field, true, warnings)
}

func decodeInt(object map[string]json.RawMessage, key string, destination *int, field string, allowZero bool, warnings *warningCollector) {
	raw, present := object[key]
	if !present {
		return
	}
	var value int
	invalid := isJSONNull(raw) || json.Unmarshal(raw, &value) != nil || value < 0 || (!allowZero && value == 0)
	if invalid {
		warnings.add(warning.WarnConfigInvalidValue, field+repairedToDefault)
		return
	}
	*destination = value
}

func decodePositiveDuration(object map[string]json.RawMessage, key string, destination *time.Duration, field string, warnings *warningCollector) {
	raw, present := object[key]
	if !present {
		return
	}
	var milliseconds int64
	if json.Unmarshal(raw, &milliseconds) != nil || milliseconds <= 0 || milliseconds > math.MaxInt64/int64(time.Millisecond) {
		warnings.add(warning.WarnConfigInvalidValue, field+repairedToDefault)
		return
	}
	*destination = time.Duration(milliseconds) * time.Millisecond
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func strictString(raw json.RawMessage) (string, bool) {
	if isJSONNull(raw) {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func strictStringSlice(operation *loadOperation, raw json.RawMessage) ([]string, bool) {
	if isJSONNull(raw) {
		return nil, false
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil {
		return nil, false
	}
	for _, value := range values {
		if operation.check() != nil {
			return nil, false
		}
		if strings.TrimSpace(value) == "" {
			return nil, false
		}
	}
	return append([]string(nil), values...), true
}

func strictBool(raw json.RawMessage) (bool, bool) {
	if isJSONNull(raw) {
		return false, false
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	return value, true
}

func decodeFraction(object map[string]json.RawMessage, key string, destination *float64, field string, warnings *warningCollector) {
	raw, present := object[key]
	if !present {
		return
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil || value <= 0 || value > 1 {
		warnings.add(warning.WarnConfigInvalidValue, field+repairedToDefault)
		return
	}
	*destination = value
}

func decodeStringSlice(operation *loadOperation, object map[string]json.RawMessage, key string, destination *[]string, field string, warnings *warningCollector) {
	raw, present := object[key]
	if !present {
		return
	}
	values, valid := decodeStringElements(operation, raw, field, warnings)
	if !valid {
		return
	}
	*destination = append([]string(nil), values...)
}

func decodeStringElements(operation *loadOperation, raw json.RawMessage, field string, warnings *warningCollector) ([]string, bool) {
	if isJSONNull(raw) {
		warnings.add(warning.WarnConfigInvalidValue, field+repaired)
		return nil, false
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		warnings.add(warning.WarnConfigInvalidValue, field+repaired)
		return nil, false
	}
	values := make([]string, 0, len(entries))
	for index, entry := range entries {
		if operation.check() != nil {
			return values, true
		}
		value, valid := strictString(entry)
		if !valid || strings.TrimSpace(value) == "" {
			warnings.add(warning.WarnConfigInvalidValue, fmt.Sprintf("%s[%d] dropped", field, index))
			continue
		}
		values = append(values, value)
	}
	return values, true
}

func strictStringMap(operation *loadOperation, raw json.RawMessage) (map[string]string, bool) {
	if isJSONNull(raw) {
		return nil, false
	}
	var encoded map[string]json.RawMessage
	if json.Unmarshal(raw, &encoded) != nil || encoded == nil {
		return nil, false
	}
	values := make(map[string]string, len(encoded))
	for key, rawValue := range encoded {
		if operation.check() != nil {
			return nil, false
		}
		value, valid := strictString(rawValue)
		if strings.TrimSpace(key) == "" || !valid {
			return nil, false
		}
		values[key] = value
	}
	return values, true
}

func decodePaths(operation *loadOperation, object map[string]json.RawMessage, key string, destination *[]string, field string, paths pathDecodeContext) error {
	raw, present := object[key]
	if !present {
		return nil
	}
	values, valid := decodeStringElements(operation, raw, field, paths.warnings)
	if !valid {
		return nil
	}
	resolved := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := operation.check(); err != nil {
			return err
		}
		path, err := normalizePath(operation, value, paths.baseDir, paths.homeDir, false)
		if err != nil {
			paths.warnings.add(warning.WarnConfigPathMissing, field+" entry dropped")
			continue
		}
		info, err := os.Stat(path)
		if contextErr := operation.check(); contextErr != nil {
			return contextErr
		}
		if err != nil || !info.IsDir() {
			paths.warnings.add(warning.WarnConfigPathMissing, field+" entry dropped")
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		resolved = append(resolved, path)
	}
	*destination = resolved
	return nil
}

func normalizeCommand(operation *loadOperation, command, baseDir, homeDir string) (string, error) {
	if err := operation.check(); err != nil {
		return "", err
	}
	if command == "" {
		return "", ErrInvalidConfig
	}
	if filepath.IsAbs(command) || command == "~" || strings.HasPrefix(command, "~/") || strings.ContainsAny(command, `/\\`) {
		path, err := normalizePath(operation, command, baseDir, homeDir, false)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if contextErr := operation.check(); contextErr != nil {
			return "", contextErr
		}
		if err != nil || !info.Mode().IsRegular() {
			return "", ErrConfigNotFound
		}
		return path, nil
	}
	return command, nil
}

func uniqueExactNames(operation *loadOperation, values []string, warnings *warningCollector) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if operation.check() != nil {
			return result
		}
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "*?[") {
			warnings.add(warning.WarnConfigAllowlistUnmatched, "non-exact MCP allowlist entry dropped")
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func uniqueStrings(operation *loadOperation, values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if operation.check() != nil {
			return result
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
