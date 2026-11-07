// Package config loads the gridtie configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

// Config is the gridtie configuration file.
type Config struct {
	Listen      string `json:"listen,omitempty"`
	AdminListen string `json:"adminListen,omitempty"`
	// Cooldown is how long a provider is skipped after it rejects a request for
	// credits or credentials (401, 402, 403).
	Cooldown Duration `json:"cooldown,omitempty"`
	// WakeOnModels wakes each model's preferred provider when a client lists models,
	// so a UI opening starts the warm-up before the first message.
	WakeOnModels bool                `json:"wakeOnModels,omitempty"`
	Providers    map[string]Provider `json:"providers"`
	Models       map[string]Model    `json:"models"`
	Rules        []Rule              `json:"rules,omitempty"`
}

// Provider is an OpenAI-compatible endpoint.
type Provider struct {
	BaseURL   string            `json:"baseURL"`
	APIKeyEnv string            `json:"apiKeyEnv,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Readiness *Readiness        `json:"readiness,omitempty"`
	Wake      *Wake             `json:"wake,omitempty"`
}

// Readiness tells gridtie whether a provider can answer right now. A provider
// without one is always ready.
type Readiness struct {
	// LlamaSwap is a llama-swap base URL; a model is ready when GET /running
	// lists it as "ready". Point it past anything that would wake the backend.
	LlamaSwap string `json:"llamaSwap,omitempty"`
}

// Wake is called, at most every 30 seconds, when a preferred provider is down.
type Wake struct {
	URL    string `json:"url"`
	Method string `json:"method,omitempty"`
}

// Model is a model clients can ask for, and where it may be served from.
type Model struct {
	Name    string   `json:"name,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	// Routes are tried in order of preference; the first is the one to wait for
	// when nothing is ready.
	Routes []Route `json:"routes"`
}

// Route serves a model from a provider under the provider's own model ID.
type Route struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Rule matches requests by Host, client CIDR or header. Matching requests with
// LocalOnly never fall back and wait for the preferred route.
type Rule struct {
	Host      string `json:"host,omitempty"`
	CIDR      string `json:"cidr,omitempty"`
	Header    string `json:"header,omitempty"`
	LocalOnly bool   `json:"localOnly,omitempty"`
}

// Duration is a time.Duration written as a string such as "10m".
type Duration struct{ time.Duration }

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse validates a config document and fills in defaults.
func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.AdminListen == "" {
		c.AdminListen = ":9090"
	}
	if c.Cooldown.Duration == 0 {
		c.Cooldown.Duration = 10 * time.Minute
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	var errs []error
	for name, p := range c.Providers {
		if u, err := url.Parse(p.BaseURL); err != nil || u.Host == "" {
			errs = append(errs, fmt.Errorf("provider %s: baseURL %q is not a URL", name, p.BaseURL))
		}
		if p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) == "" {
			errs = append(errs, fmt.Errorf("provider %s: %s is not set", name, p.APIKeyEnv))
		}
		if p.Wake != nil && p.Wake.URL == "" {
			errs = append(errs, fmt.Errorf("provider %s: wake needs a url", name))
		}
	}
	if len(c.Models) == 0 {
		errs = append(errs, errors.New("no models"))
	}
	seen := map[string]string{}
	for id, m := range c.Models {
		if len(m.Routes) == 0 {
			errs = append(errs, fmt.Errorf("model %s: no routes", id))
		}
		for _, r := range m.Routes {
			if _, ok := c.Providers[r.Provider]; !ok {
				errs = append(errs, fmt.Errorf("model %s: unknown provider %q", id, r.Provider))
			}
			if r.Model == "" {
				errs = append(errs, fmt.Errorf("model %s: route to %s has no model", id, r.Provider))
			}
		}
		for _, name := range append([]string{id}, m.Aliases...) {
			if other, ok := seen[name]; ok {
				errs = append(errs, fmt.Errorf("model %s: name %q is also used by %s", id, name, other))
			}
			seen[name] = id
		}
	}
	for i, r := range c.Rules {
		n := 0
		for _, s := range []string{r.Host, r.CIDR, r.Header} {
			if s != "" {
				n++
			}
		}
		if n != 1 {
			errs = append(errs, fmt.Errorf("rule %d: set exactly one of host, cidr, header", i))
		}
	}
	return errors.Join(errs...)
}
