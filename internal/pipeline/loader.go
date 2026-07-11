package pipeline

import "fmt"

// PluginSpec is the config-driven description of one plugin entry.
// Used by loaders to resolve and construct plugins at startup.
type PluginSpec struct {
	Name      string         // registered plugin name
	ExecModel string         // "builtin" | "wasm" | "grpc"
	Priority  int            // overrides Plugin.Priority() when > 0
	Config    map[string]any // plugin-specific config values
}

// PluginLoader resolves a PluginSpec into a live Plugin.
type PluginLoader interface {
	Load(spec PluginSpec) (Plugin, error)
}

// BuiltinLoader resolves PluginSpecs to built-in Go plugins via a name registry.
// WASMLoader and GRPCLoader are added in later epics.
type BuiltinLoader struct {
	registry map[string]func(spec PluginSpec) (Plugin, error)
}

// NewBuiltinLoader creates an empty BuiltinLoader.
func NewBuiltinLoader() *BuiltinLoader {
	return &BuiltinLoader{
		registry: make(map[string]func(PluginSpec) (Plugin, error)),
	}
}

// Register maps a plugin name to a factory function.
// Factories are called once per PluginSpec at startup.
func (l *BuiltinLoader) Register(name string, factory func(spec PluginSpec) (Plugin, error)) {
	l.registry[name] = factory
}

// Load resolves spec.Name via the registry and calls the factory.
func (l *BuiltinLoader) Load(spec PluginSpec) (Plugin, error) {
	factory, ok := l.registry[spec.Name]
	if !ok {
		return nil, fmt.Errorf("no builtin plugin registered for name %q", spec.Name)
	}
	return factory(spec)
}
