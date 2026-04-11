package llm

import "fmt"

// Factory constructs a Provider from an API key and options.
type Factory func(apiKey string, opts ...Option) Provider

var registry = map[string]Factory{
	"anthropic": NewAnthropicProvider,
	"openai":    NewOpenAIProvider,
}

// NewProvider looks up a factory by name and constructs a provider.
func NewProvider(name, apiKey string, opts ...Option) (Provider, error) {
	factory, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown llm provider: %q", name)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("empty api key for provider %q", name)
	}
	return factory(apiKey, opts...), nil
}

// Register installs a provider factory under the given name. Later calls
// overwrite earlier ones; this is intended for tests and third-party
// extensions.
func Register(name string, f Factory) {
	registry[name] = f
}
