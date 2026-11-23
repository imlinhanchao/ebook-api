package providers

import (
	"fmt"
	"sort"
	"sync"
)

// Registry holds the active book-source plugins.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry creates an empty provider registry.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

// Register adds or replaces a provider keyed by p.ID().
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.ID()] = p
}

// Get returns the provider with the given ID.
func (r *Registry) Get(id string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	return p, ok
}

// List returns all registered providers sorted by ID.
func (r *Registry) List() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// IDs returns the sorted IDs of all registered providers.
func (r *Registry) IDs() []string {
	list := r.List()
	ids := make([]string, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.ID())
	}
	return ids
}

// Describe builds a ProviderInfo from p, filling defaults when p does not
// implement InfoProvider.
func Describe(p Provider) ProviderInfo {
	if ip, ok := p.(InfoProvider); ok {
		info := ip.Info()
		info.ID = p.ID()
		if info.Name == "" {
			info.Name = p.DisplayName()
		}
		if info.Capabilities == nil {
			info.Capabilities = p.Capabilities()
		}
		return info
	}
	return ProviderInfo{
		ID:           p.ID(),
		Name:         p.DisplayName(),
		Capabilities: p.Capabilities(),
	}
}

// ErrUnknownProvider returns an ErrUnsupported wrapping the unknown ID.
func ErrUnknownProvider(id string) error {
	return fmt.Errorf("%w: unknown provider %q", ErrUnsupported, id)
}
