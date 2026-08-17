package plasmid

// Plugin is a host-compiled extension initialized during Harness construction.
// Init may register native tools, toolsets, and ADK plugins on the supplied
// Harness. Registration is sealed before New returns.
type Plugin interface {
	Name() string
	Init(*Harness) error
	Close() error
}
