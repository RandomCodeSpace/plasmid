package plasmid

// PromptFragment is one named, static instruction fragment supplied by a
// host-compiled plugin during Init.
type PromptFragment struct {
	Name    string
	Content string
}

// Plugin is a host-compiled extension initialized during Harness construction.
// Init may register native tools, toolsets, and ADK plugins on the supplied
// Harness. Registration is sealed before New returns.
type Plugin interface {
	Name() string
	Init(*Harness) error
	Close() error
}
