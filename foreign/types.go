// Package foreign discovers inert extension metadata installed for supported
// coding-agent hosts. Discovery never activates an extension or executes code.
package foreign

import "github.com/plasmid-dev/plasmid/warning"

// Host identifies one independently ordered foreign ecosystem.
type Host string

const (
	HostClaude  Host = "claude"
	HostCodex   Host = "codex"
	HostCopilot Host = "copilot"
)

// Scope identifies where a host found an extension.
type Scope string

const (
	ScopeProject Scope = "project"
	ScopeLocal   Scope = "local"
	ScopeUser    Scope = "user"
	ScopeAdmin   Scope = "admin"
)

// Classification distinguishes supported contracts from compatibility input.
type Classification string

const (
	ClassificationDocumented    Classification = "documented"
	ClassificationCompatibility Classification = "compatibility"
	ClassificationPreview       Classification = "preview"
)

// Trust records host-supplied repository trust without granting authority.
type Trust string

const (
	TrustUnknown   Trust = "unknown"
	TrustTrusted   Trust = "trusted"
	TrustUntrusted Trust = "untrusted"
)

// MetadataEntry is one portable Agent Skills metadata value.
type MetadataEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ToolPattern is retained foreign permission metadata. Discovery never grants
// the permission or turns it into a Plasmid tool policy.
type ToolPattern struct {
	Tool     string `json:"tool"`
	Argument string `json:"argument"`
}

// InertPermissions retains foreign allow and deny declarations for inspection.
type InertPermissions struct {
	Allowed []ToolPattern `json:"allowed"`
	Denied  []ToolPattern `json:"denied"`
}

// Provenance records every source collapsed into one catalog record.
type Provenance struct {
	Host           Host           `json:"host"`
	Scope          Scope          `json:"scope"`
	SourcePath     string         `json:"source_path"`
	PluginID       string         `json:"plugin_id"`
	PluginVersion  string         `json:"plugin_version"`
	Enabled        bool           `json:"enabled"`
	Trust          Trust          `json:"trust"`
	Classification Classification `json:"classification"`
}

// Skill is the portable Agent Skills core. Host permission fields are omitted.
type Skill struct {
	Name          string           `json:"name"`
	QualifiedName string           `json:"qualified_name"`
	Description   string           `json:"description"`
	License       string           `json:"license"`
	Compatibility string           `json:"compatibility"`
	Metadata      []MetadataEntry  `json:"metadata"`
	Permissions   InertPermissions `json:"inert_permissions"`
	Body          string           `json:"body"`
	Provenance    []Provenance     `json:"provenance"`
	sourceDigest  string
	realPaths     []string
}

// Template is an explicit-invocation legacy command or prompt.
type Template struct {
	Name          string       `json:"name"`
	QualifiedName string       `json:"qualified_name"`
	Body          string       `json:"body"`
	Provenance    []Provenance `json:"provenance"`
}

// MCPServer is inert discovery metadata. It deliberately excludes credentials,
// environment values, headers, and executable arguments.
type MCPServer struct {
	Name          string       `json:"name"`
	QualifiedName string       `json:"qualified_name"`
	Transport     string       `json:"transport"`
	Inert         bool         `json:"inert"`
	Provenance    []Provenance `json:"provenance"`
}

// HostCatalog retains one host's independent ordering and precedence.
type HostCatalog struct {
	Host       Host              `json:"host"`
	Skills     []Skill           `json:"skills"`
	Templates  []Template        `json:"templates"`
	MCPServers []MCPServer       `json:"mcp_servers"`
	Warnings   []warning.Warning `json:"warnings"`
}

// Catalog exposes normalized skills and independent host source views without
// defining cross-host precedence.
type Catalog struct {
	Hosts    []HostCatalog     `json:"hosts"`
	Skills   []Skill           `json:"skills"`
	Warnings []warning.Warning `json:"warnings"`
}

// Options selects filesystem roots and bounded discovery behavior.
type Options struct {
	HomeDir              string
	WorkingDir           string
	RepositoryRoot       string
	CodexHome            string
	AdminSkillsDir       string
	ProjectTrusted       bool
	EnableCopilotPreview bool
	MaxEntries           int
	MaxFileBytes         int64
}
