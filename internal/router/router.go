// Package router decides, per request, which providers to try and in what order.
package router

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/tpural/gridtie/internal/config"
	"github.com/tpural/gridtie/internal/provider"
)

// HeaderRoute lets a client opt out of fallbacks: "local-only" (or "wait") sends
// the request to the preferred route even when it is cold.
const HeaderRoute = "X-Gridtie-Route"

// Model is a model clients can ask for.
type Model struct {
	ID      string
	Name    string
	Aliases []string
	Routes  []config.Route
}

// Attempt is one provider to try for a request.
type Attempt struct {
	Provider *provider.Provider
	Model    string
	State    provider.State
}

// Router picks providers for requests.
type Router struct {
	providers map[string]*provider.Provider
	models    []*Model
	byName    map[string]*Model
	rules     []rule
}

type rule struct {
	localOnly bool
	match     func(*http.Request) bool
}

// New builds a router from the config and its providers.
func New(c *config.Config, providers map[string]*provider.Provider) (*Router, error) {
	r := &Router{providers: providers, byName: map[string]*Model{}}
	for id, m := range c.Models {
		model := &Model{ID: id, Name: m.Name, Aliases: m.Aliases, Routes: m.Routes}
		r.models = append(r.models, model)
		for _, name := range append([]string{id}, m.Aliases...) {
			r.byName[name] = model
		}
	}
	slices.SortFunc(r.models, func(a, b *Model) int { return strings.Compare(a.ID, b.ID) })
	for i, c := range c.Rules {
		match, err := matcher(c)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
		r.rules = append(r.rules, rule{localOnly: c.LocalOnly, match: match})
	}
	return r, nil
}

// Models lists the models in ID order.
func (r *Router) Models() []*Model { return r.models }

// Lookup finds a model by ID or alias.
func (r *Router) Lookup(name string) (*Model, bool) {
	m, ok := r.byName[name]
	return m, ok
}

// LocalOnly reports whether the request must wait for the preferred route.
func (r *Router) LocalOnly(req *http.Request) bool {
	switch strings.ToLower(req.Header.Get(HeaderRoute)) {
	case "local-only", "wait":
		return true
	}
	for _, rl := range r.rules {
		if rl.match(req) {
			return rl.localOnly
		}
	}
	return false
}

// Plan returns the providers to try, in order. Routes whose context window is
// too small for tokens are dropped, unless none is big enough. Ready routes come
// first in preference order, skipping providers that are cooling down; the
// preferred route is always last if it isn't first, so the request waits for it
// when everything else fails. A preferred route that isn't ready is woken.
// localOnly sends the request to the model's first route, whatever its state.
func (r *Router) Plan(ctx context.Context, m *Model, localOnly bool, tokens int) []Attempt {
	routes := m.Routes
	if !localOnly {
		routes = fitting(routes, tokens)
	}
	first := routes[0]
	fp := r.providers[first.Provider]
	state := fp.State(ctx, first.Model)
	if state != provider.Ready {
		fp.Wake(state, first.Model)
	}
	preferred := Attempt{Provider: fp, Model: first.Model, State: state}
	if localOnly {
		return []Attempt{preferred}
	}
	var out []Attempt
	if cooling, _, _ := fp.Cooling(); state == provider.Ready && !cooling {
		out = append(out, preferred)
	}
	for _, rt := range routes[1:] {
		p := r.providers[rt.Provider]
		if cooling, _, _ := p.Cooling(); cooling {
			continue
		}
		if s := p.State(ctx, rt.Model); s == provider.Ready {
			out = append(out, Attempt{Provider: p, Model: rt.Model, State: s})
		}
	}
	if len(out) == 0 || out[0].Provider != fp || out[0].Model != first.Model {
		out = append(out, preferred)
	}
	return out
}

func fitting(routes []config.Route, tokens int) []config.Route {
	var out []config.Route
	for _, rt := range routes {
		if rt.MaxContext == 0 || tokens <= rt.MaxContext {
			out = append(out, rt)
		}
	}
	if len(out) == 0 {
		return routes
	}
	return out
}

// WakePreferred wakes the preferred provider of every model that is down,
// without loading models, e.g. when a UI lists models.
func (r *Router) WakePreferred(ctx context.Context) {
	for _, m := range r.models {
		first := m.Routes[0]
		p := r.providers[first.Provider]
		if s := p.State(ctx, first.Model); s == provider.Down {
			p.Wake(s, first.Model)
		}
	}
}

func matcher(c config.Rule) (func(*http.Request) bool, error) {
	switch {
	case c.Host != "":
		return func(req *http.Request) bool { return strings.EqualFold(hostOnly(req.Host), c.Host) }, nil
	case c.CIDR != "":
		prefix, err := netip.ParsePrefix(c.CIDR)
		if err != nil {
			return nil, err
		}
		return func(req *http.Request) bool {
			ip, err := netip.ParseAddr(hostOnly(req.RemoteAddr))
			return err == nil && prefix.Contains(ip.Unmap())
		}, nil
	default:
		name, expr, ok := strings.Cut(c.Header, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("header %q must be <Name>=<regexp>", c.Header)
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, err
		}
		return func(req *http.Request) bool { return re.MatchString(req.Header.Get(name)) }, nil
	}
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}
