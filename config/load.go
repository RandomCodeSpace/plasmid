package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/plasmid-dev/plasmid/warning"
)

const maxConfigBytes = 1 << 20

var (
	ErrConfigNotFound     = errors.New("config file not found")
	ErrInvalidConfig      = errors.New("invalid config")
	ErrUnsupportedVersion = errors.New("unsupported config version")
)

// Load discovers, reads, repairs, and validates one config file. Options are
// applied after the file and therefore represent Harness functional-option
// precedence.
func Load(ctx context.Context, options Options) (Result, error) {
	operation := loadOperation{}
	if ctx != nil {
		operation.contextError = ctx.Err
	}
	if err := operation.check(); err != nil {
		return Result{}, err
	}
	workingDir, err := resolveWorkingDir(&operation, options.WorkingDir)
	if err != nil {
		return Result{}, err
	}
	if err := operation.check(); err != nil {
		return Result{}, err
	}
	homeDir, _ := os.UserHomeDir()
	if err := operation.check(); err != nil {
		return Result{}, err
	}
	defaultConfiguration := defaults(workingDir)
	configuration := cloneConfig(defaultConfiguration)
	sourcePath, found, err := discover(&operation, options.ConfigPath, workingDir, homeDir)
	if err != nil {
		return Result{}, err
	}
	collector := warningCollector{path: filepath.ToSlash(sourcePath)}
	if found {
		data, readErr := readBounded(&operation, sourcePath)
		if readErr != nil {
			return Result{}, fmt.Errorf("read config %q: %w", sourcePath, readErr)
		}
		if decodeErr := decodeFile(&operation, data, &configuration, defaultConfiguration, filepath.Dir(sourcePath), homeDir, &collector); decodeErr != nil {
			return Result{}, fmt.Errorf("decode config %q: %w", sourcePath, decodeErr)
		}
	}
	if err := operation.check(); err != nil {
		return Result{}, err
	}
	if err := applyOptions(&operation, &configuration, options, workingDir); err != nil {
		return Result{}, err
	}
	if err := operation.check(); err != nil {
		return Result{}, err
	}
	return Result{
		Config:     cloneConfig(configuration),
		SourcePath: sourcePath,
		Warnings:   append([]warning.Warning(nil), collector.values...),
	}, nil
}

type loadOperation struct {
	contextError func() error
}

func (o *loadOperation) check() error {
	if o == nil || o.contextError == nil {
		return errors.New("nil context")
	}
	return o.contextError()
}

func resolveWorkingDir(operation *loadOperation, value string) (string, error) {
	if err := operation.check(); err != nil {
		return "", err
	}
	if value == "" {
		var err error
		value, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		if err := operation.check(); err != nil {
			return "", err
		}
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve working directory %q: %w", value, err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if contextErr := operation.check(); contextErr != nil {
		return "", contextErr
	}
	if err != nil {
		return "", fmt.Errorf("resolve working directory %q: %w", value, err)
	}
	info, err := os.Stat(resolved)
	if contextErr := operation.check(); contextErr != nil {
		return "", contextErr
	}
	if err != nil {
		return "", fmt.Errorf("inspect working directory %q: %w", value, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory %q: %w", value, ErrInvalidConfig)
	}
	return filepath.Clean(resolved), nil
}

func discover(operation *loadOperation, explicit, workingDir, homeDir string) (string, bool, error) {
	if err := operation.check(); err != nil {
		return "", false, err
	}
	if explicit != "" {
		return discoverExplicit(operation, explicit, workingDir, homeDir)
	}
	return discoverFirst(operation, discoveryCandidates(workingDir, homeDir))
}

func discoverExplicit(operation *loadOperation, explicit, workingDir, homeDir string) (string, bool, error) {
	path, err := normalizePath(operation, explicit, workingDir, homeDir, true)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("%w: %s: %w", ErrConfigNotFound, explicit, err)
		}
		return "", false, fmt.Errorf("resolve explicit config path: %w", err)
	}
	resolved, err := resolveConfigCandidate(operation, path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("%w: %s: %w", ErrConfigNotFound, path, err)
	}
	if err != nil {
		return "", false, err
	}
	return resolved, true, nil
}

func discoveryCandidates(workingDir, homeDir string) []string {
	candidates := []string{filepath.Join(workingDir, ".plasmid.json")}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		candidates = append(candidates, filepath.Join(filepath.Clean(xdg), "plasmid", "config.json"))
	}
	if homeDir != "" {
		candidates = append(candidates, filepath.Join(homeDir, ".config", "plasmid", "config.json"))
	}
	return candidates
}

func discoverFirst(operation *loadOperation, candidates []string) (string, bool, error) {
	for _, candidate := range candidates {
		if err := operation.check(); err != nil {
			return "", false, err
		}
		resolved, err := resolveConfigCandidate(operation, candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		return resolved, true, nil
	}
	return "", false, nil
}

func resolveConfigCandidate(operation *loadOperation, path string) (string, error) {
	info, err := os.Stat(path)
	if contextErr := operation.check(); contextErr != nil {
		return "", contextErr
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", fmt.Errorf("inspect config %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("config %q is not a regular file: %w", path, ErrInvalidConfig)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if contextErr := operation.check(); contextErr != nil {
		return "", contextErr
	}
	if err != nil {
		return "", fmt.Errorf("resolve config %q: %w", path, err)
	}
	return filepath.Clean(resolved), nil
}

func readBounded(operation *loadOperation, path string) (data []byte, err error) {
	if err := operation.check(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if contextErr := operation.check(); contextErr != nil {
		if file != nil {
			contextErr = errors.Join(contextErr, file.Close())
		}
		return nil, contextErr
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	data, err = io.ReadAll(io.LimitReader(contextReader{operation: operation, reader: file}, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("config exceeds %d bytes: %w", maxConfigBytes, ErrInvalidConfig)
	}
	return data, nil
}

type contextReader struct {
	operation *loadOperation
	reader    io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.operation.check(); err != nil {
		return 0, err
	}
	count, err := r.reader.Read(buffer)
	if contextErr := r.operation.check(); contextErr != nil {
		return count, contextErr
	}
	return count, err
}

func applyOptions(operation *loadOperation, configuration *Config, options Options, workingDir string) error {
	if err := operation.check(); err != nil {
		return err
	}
	configuration.WorkingDir = workingDir
	if options.SessionDir != "" {
		configuration.SessionDir = options.SessionDir
	}
	path, err := normalizePath(operation, configuration.SessionDir, workingDir, "", true)
	if err != nil {
		return fmt.Errorf("resolve session directory: %w", err)
	}
	configuration.SessionDir = path
	if options.UserID != "" {
		configuration.UserID = options.UserID
	}
	if options.AppName != nil {
		configuration.AppName = *options.AppName
	}
	if options.LSPMode != nil {
		configuration.LSP.Mode = *options.LSPMode
	}
	if options.Foreign != nil {
		configuration.Foreign = cloneForeign(*options.Foreign)
	}
	if options.ToolConfirmation != nil {
		configuration.Tools.Confirmation = *options.ToolConfirmation
	}
	if err := normalizeOptionPaths(operation, configuration, workingDir); err != nil {
		return err
	}
	if err := validateOptions(*configuration); err != nil {
		return fmt.Errorf("functional option: %w", err)
	}
	return nil
}

func normalizeOptionPaths(operation *loadOperation, configuration *Config, workingDir string) error {
	var err error
	if configuration.Skills.Roots, err = normalizeDirectories(operation, configuration.Skills.Roots, workingDir); err != nil {
		return fmt.Errorf("resolve skill root: %w", err)
	}
	if configuration.Foreign.TrustedRoots, err = normalizeDirectories(operation, configuration.Foreign.TrustedRoots, workingDir); err != nil {
		return fmt.Errorf("resolve trusted root: %w", err)
	}
	if configuration.Context.ImportRoots, err = normalizeDirectories(operation, configuration.Context.ImportRoots, workingDir); err != nil {
		return fmt.Errorf("resolve import root: %w", err)
	}
	if err := normalizeLSPCommands(operation, configuration.LSP.Servers, workingDir); err != nil {
		return err
	}
	return normalizeMCPCommands(operation, configuration.MCP.Servers, workingDir)
}

func normalizeDirectories(operation *loadOperation, values []string, workingDir string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := operation.check(); err != nil {
			return nil, err
		}
		path, err := normalizePath(operation, value, workingDir, "", true)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if contextErr := operation.check(); contextErr != nil {
			return nil, contextErr
		}
		if err != nil || !info.IsDir() {
			return nil, ErrConfigNotFound
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result, nil
}

func normalizeLSPCommands(operation *loadOperation, servers []LSPServer, workingDir string) error {
	for index := range servers {
		if err := operation.check(); err != nil {
			return err
		}
		command, err := normalizeCommand(operation, servers[index].Command, workingDir, "")
		if err != nil {
			return fmt.Errorf("resolve LSP server %q: %w", servers[index].ID, err)
		}
		servers[index].Command = command
	}
	return nil
}

func normalizeMCPCommands(operation *loadOperation, servers []MCPServer, workingDir string) error {
	for index := range servers {
		if err := operation.check(); err != nil {
			return err
		}
		if servers[index].Transport != MCPStdio {
			continue
		}
		command, err := normalizeCommand(operation, servers[index].Command, workingDir, "")
		if err != nil {
			return fmt.Errorf("resolve MCP server %q: %w", servers[index].ID, err)
		}
		servers[index].Command = command
	}
	return nil
}

func normalizePath(operation *loadOperation, value, baseDir, homeDir string, allowMissing bool) (string, error) {
	if err := operation.check(); err != nil {
		return "", err
	}
	value, err := expandPath(operation, value, baseDir, homeDir)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	resolved, err := filepath.EvalSymlinks(absolute)
	if contextErr := operation.check(); contextErr != nil {
		return "", contextErr
	}
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !allowMissing || !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return resolveMissingPath(operation, absolute)
}

func expandPath(operation *loadOperation, value, baseDir, homeDir string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("empty path: %w", ErrInvalidConfig)
	}
	if value != "~" && !strings.HasPrefix(value, "~/") {
		if !filepath.IsAbs(value) {
			value = filepath.Join(baseDir, value)
		}
		return value, nil
	}
	if homeDir == "" {
		resolvedHome, err := os.UserHomeDir()
		if contextErr := operation.check(); contextErr != nil {
			return "", contextErr
		}
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		homeDir = resolvedHome
	}
	return filepath.Join(homeDir, strings.TrimPrefix(value, "~/")), nil
}

func resolveMissingPath(operation *loadOperation, absolute string) (string, error) {
	probe := absolute
	missing := make([]string, 0, 2)
	for {
		if err := operation.check(); err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(probe)
		if contextErr := operation.check(); contextErr != nil {
			return "", contextErr
		}
		if err == nil {
			return reattachMissingPath(resolved, missing)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if checkErr := checkMissingComponent(operation, probe, err); checkErr != nil {
			return "", checkErr
		}

		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

func checkMissingComponent(operation *loadOperation, path string, evalErr error) error {
	if err := operation.check(); err != nil {
		return err
	}
	_, err := os.Lstat(path)
	if contextErr := operation.check(); contextErr != nil {
		return contextErr
	}
	if err == nil {
		return evalErr
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func reattachMissingPath(resolved string, missing []string) (string, error) {
	resolved = filepath.Clean(resolved)
	candidate := resolved
	for index := len(missing) - 1; index >= 0; index-- {
		candidate = filepath.Join(candidate, missing[index])
	}
	contained, err := filepath.Rel(resolved, candidate)
	if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("reattach missing path suffix: %w", ErrInvalidConfig)
	}
	return filepath.Clean(candidate), nil
}

type warningCollector struct {
	path   string
	values []warning.Warning
}

func (c *warningCollector) add(code, message string) {
	c.values = append(c.values, warning.Warning{Code: code, Source: "config", Path: c.path, Message: message})
}

// Clone returns a deep copy of the configuration's mutable collections.
func (value Config) Clone() Config {
	return cloneConfig(value)
}

func cloneConfig(value Config) Config {
	value.LSP = cloneLSP(value.LSP)
	value.MCP = cloneMCP(value.MCP)
	value.Skills = cloneSkills(value.Skills)
	value.Foreign = cloneForeign(value.Foreign)
	value.Context = cloneContext(value.Context)
	value.Compaction = cloneCompaction(value.Compaction)
	return value
}

func cloneLSP(value LSP) LSP {
	value.Servers = append([]LSPServer(nil), value.Servers...)
	for index := range value.Servers {
		value.Servers[index].Args = append([]string(nil), value.Servers[index].Args...)
		value.Servers[index].Extensions = append([]string(nil), value.Servers[index].Extensions...)
		value.Servers[index].RootMarkers = append([]string(nil), value.Servers[index].RootMarkers...)
	}
	return value
}

func cloneMCP(value MCP) MCP {
	value.AllowForeign = append([]string(nil), value.AllowForeign...)
	value.Servers = append([]MCPServer(nil), value.Servers...)
	for index := range value.Servers {
		value.Servers[index].Args = append([]string(nil), value.Servers[index].Args...)
		value.Servers[index].Env = cloneMap(value.Servers[index].Env)
		value.Servers[index].Headers = cloneMap(value.Servers[index].Headers)
	}
	return value
}

func cloneMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func cloneSkills(value Skills) Skills {
	value.Roots = append([]string(nil), value.Roots...)
	return value
}

func cloneForeign(value Foreign) Foreign {
	value.TrustedRoots = append([]string(nil), value.TrustedRoots...)
	return value
}

func cloneContext(value Context) Context {
	value.ImportRoots = append([]string(nil), value.ImportRoots...)
	return value
}

func cloneCompaction(value Compaction) Compaction {
	value.PreserveToolNames = append([]string(nil), value.PreserveToolNames...)
	return value
}

func validateOptions(value Config) error {
	if strings.TrimSpace(value.AppName) == "" || strings.TrimSpace(value.UserID) == "" {
		return fmt.Errorf("app name and user ID must be non-empty: %w", ErrInvalidConfig)
	}
	if value.LSP.Mode != LSPAuto && value.LSP.Mode != LSPOff {
		return fmt.Errorf("invalid LSP mode %q: %w", value.LSP.Mode, ErrInvalidConfig)
	}
	return nil
}

func decodeFile(operation *loadOperation, data []byte, configuration *Config, fallback Config, baseDir, homeDir string, warnings *warningCollector) error {
	if err := operation.check(); err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	if err := operation.check(); err != nil {
		return err
	}
	if top == nil {
		return fmt.Errorf("top-level value must be an object: %w", ErrInvalidConfig)
	}
	return decodeTop(operation, top, configuration, fallback, baseDir, homeDir, warnings)
}
