// Package provider tracks whether each upstream can answer right now and wakes
// the ones that can't.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tpural/gridtie/internal/config"
)

// State is a provider's readiness for one model.
type State string

const (
	// Ready can answer now.
	Ready State = "ready"
	// Starting is loading the model.
	Starting State = "starting"
	// Idle is up but has a different model loaded.
	Idle State = "idle"
	// Down is not reachable, e.g. asleep or scaled to zero.
	Down State = "down"
)

const (
	cacheFor    = 2 * time.Second
	probeFor    = 2 * time.Second
	wakeEvery   = 30 * time.Second
	wakeTimeout = 30 * time.Second
)

// Provider is one OpenAI-compatible upstream.
type Provider struct {
	Name    string
	BaseURL *url.URL
	APIKey  string
	Headers map[string]string

	llamaSwap *url.URL
	wake      *config.Wake
	client    *http.Client
	log       *slog.Logger

	probeMu   sync.Mutex
	running   map[string]string
	reachable bool
	checked   time.Time

	mu        sync.Mutex
	coolUntil time.Time
	coolWhy   string
	woken     map[string]time.Time
}

// New builds a provider from its config.
func New(name string, c config.Provider, client *http.Client, log *slog.Logger) (*Provider, error) {
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil {
		return nil, err
	}
	p := &Provider{
		Name:    name,
		BaseURL: base,
		Headers: c.Headers,
		wake:    c.Wake,
		client:  client,
		log:     log.With("provider", name),
		woken:   map[string]time.Time{},
	}
	if c.APIKeyEnv != "" {
		p.APIKey = os.Getenv(c.APIKeyEnv)
	}
	if c.Readiness != nil && c.Readiness.LlamaSwap != "" {
		if p.llamaSwap, err = url.Parse(strings.TrimRight(c.Readiness.LlamaSwap, "/")); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// State reports whether the provider can serve model now. Providers without a
// readiness check are always ready.
func (p *Provider) State(ctx context.Context, model string) State {
	if p.llamaSwap == nil {
		return Ready
	}
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	if time.Since(p.checked) > cacheFor {
		p.running, p.reachable = p.fetchRunning(ctx)
		p.checked = time.Now()
	}
	if !p.reachable {
		return Down
	}
	switch p.running[model] {
	case "ready":
		return Ready
	case "starting":
		return Starting
	default:
		return Idle
	}
}

func (p *Provider) fetchRunning(ctx context.Context) (map[string]string, bool) {
	ctx, cancel := context.WithTimeout(ctx, probeFor)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.llamaSwap.String()+"/running", http.NoBody)
	if err != nil {
		return nil, false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var body struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil, false
	}
	out := make(map[string]string, len(body.Running))
	for _, m := range body.Running {
		out[m.Model] = m.State
	}
	return out, true
}

// Wake nudges the provider towards serving model, in the background and at most
// every 30 seconds per model: the wake hook when it is down, and a model load
// when it is up with another model.
func (p *Provider) Wake(state State, model string) {
	var target, method string
	switch {
	case state == Down && p.wake != nil:
		target, method = p.wake.URL, p.wake.Method
		model = ""
	case state == Idle && p.llamaSwap != nil:
		target, method = p.llamaSwap.String()+"/upstream/"+url.PathEscape(model)+"/health", http.MethodGet
	default:
		return
	}
	if method == "" {
		method = http.MethodPost
	}
	p.mu.Lock()
	if time.Since(p.woken[model]) < wakeEvery {
		p.mu.Unlock()
		return
	}
	p.woken[model] = time.Now()
	p.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), wakeTimeout)
		defer cancel()
		if err := p.call(ctx, method, target); err != nil {
			p.log.Warn("wake failed", "state", state, "model", model, "err", err)
			return
		}
		p.log.Info("woke", "state", state, "model", model)
	}()
}

func (p *Provider) call(ctx context.Context, method, target string) error {
	req, err := http.NewRequestWithContext(ctx, method, target, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil && method == http.MethodGet {
			return nil // a model load outlasting the timeout is fine; it keeps loading
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, target, resp.Status)
	}
	return nil
}

// Cool skips the provider until the given time, e.g. after it ran out of credits.
func (p *Provider) Cool(until time.Time, why string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if until.After(p.coolUntil) {
		p.coolUntil, p.coolWhy = until, why
		p.log.Warn("cooling down", "until", until.UTC().Format(time.RFC3339), "why", why)
	}
}

// Cooling reports whether the provider is being skipped, and why.
func (p *Provider) Cooling() (bool, string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Now().Before(p.coolUntil) {
		return true, p.coolWhy, p.coolUntil
	}
	return false, "", time.Time{}
}
