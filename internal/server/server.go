// Package server serves the OpenAI-compatible API and forwards each request to
// the first provider that can answer it.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tpural/gridtie/internal/provider"
	"github.com/tpural/gridtie/internal/router"
)

const (
	maxBody        = 64 << 20
	defaultBackoff = time.Minute
	// HeaderProvider names the provider that answered.
	HeaderProvider = "X-Gridtie-Provider"
)

// hopHeaders are connection-specific and never forwarded.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Options configures a Server.
type Options struct {
	Router       *router.Router
	Client       *http.Client
	Cooldown     time.Duration
	WakeOnModels bool
	Metrics      *Metrics
	Log          *slog.Logger
}

// Server is the client-facing API.
type Server struct {
	opts Options
	mux  *http.ServeMux
}

// New returns a Server.
func New(opts Options) *Server {
	if opts.Metrics == nil {
		opts.Metrics = NewMetrics(prometheus.NewRegistry())
	}
	s := &Server{opts: opts, mux: http.NewServeMux()}
	for _, p := range []string{"/v1/models", "/models"} {
		s.mux.HandleFunc("GET "+p, s.models)
	}
	for _, p := range []string{"/chat/completions", "/completions", "/embeddings"} {
		s.mux.HandleFunc("POST /v1"+p, s.forwardTo(p))
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	if s.opts.WakeOnModels {
		go s.opts.Router.WakePreferred(context.Background())
	}
	type entry struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		Created int64    `json:"created"`
		OwnedBy string   `json:"owned_by"`
		Name    string   `json:"name,omitempty"`
		Aliases []string `json:"aliases,omitempty"`
	}
	list := struct {
		Object string  `json:"object"`
		Data   []entry `json:"data"`
	}{Object: "list", Data: []entry{}}
	for _, m := range s.opts.Router.Models() {
		list.Data = append(list.Data, entry{ID: m.ID, Object: "model", OwnedBy: "gridtie", Name: m.Name, Aliases: m.Aliases})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (s *Server) forwardTo(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.forward(w, r, path) }
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, path string) {
	start := time.Now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, "request body too large or unreadable")
		return
	}
	var fields map[string]json.RawMessage
	var name string
	if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["model"], &name) != nil {
		apiError(w, http.StatusBadRequest, "request body must be a JSON object with a model")
		return
	}
	m, ok := s.opts.Router.Lookup(name)
	if !ok {
		apiError(w, http.StatusNotFound, "model "+strconv.Quote(name)+" does not exist")
		return
	}
	localOnly := s.opts.Router.LocalOnly(r)
	attempts := s.opts.Router.Plan(r.Context(), m, localOnly)
	log := s.opts.Log.With("model", m.ID, "localOnly", localOnly)

	var skipped []string
	for i, a := range attempts {
		last := i == len(attempts)-1
		resp, err := s.send(r, a, path, fields)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			s.opts.Metrics.skips.WithLabelValues(a.Provider.Name, "error").Inc()
			if last {
				log.Error("no provider answered", "provider", a.Provider.Name, "err", err, "skipped", skipped)
				apiError(w, http.StatusBadGateway, "no provider could answer: "+err.Error())
				return
			}
			skipped = append(skipped, a.Provider.Name+": "+err.Error())
			continue
		}
		if rejected(resp.StatusCode) {
			s.cool(a.Provider, resp)
			if !last {
				s.opts.Metrics.skips.WithLabelValues(a.Provider.Name, strconv.Itoa(resp.StatusCode)).Inc()
				skipped = append(skipped, a.Provider.Name+": "+resp.Status)
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				_ = resp.Body.Close()
				continue
			}
		}
		code := strconv.Itoa(resp.StatusCode)
		s.opts.Metrics.requests.WithLabelValues(m.ID, a.Provider.Name, code).Inc()
		log.Info("request", "provider", a.Provider.Name, "upstreamModel", a.Model, "state", a.State,
			"status", resp.StatusCode, "skipped", skipped, "wait", time.Since(start).Round(time.Millisecond))
		s.relay(w, resp, a)
		s.opts.Metrics.seconds.WithLabelValues(a.Provider.Name).Observe(time.Since(start).Seconds())
		return
	}
}

func (s *Server) send(r *http.Request, a router.Attempt, path string, fields map[string]json.RawMessage) (*http.Response, error) {
	model, _ := json.Marshal(a.Model)
	out := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		out[k] = v
	}
	out["model"] = model
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.Provider.BaseURL.String()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	for h := range req.Header {
		if strings.HasPrefix(strings.ToLower(h), "x-gridtie-") {
			req.Header.Del(h)
		}
	}
	req.Header.Del("Accept-Encoding")
	req.Header.Del("Content-Length")
	if a.Provider.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.Provider.APIKey)
	}
	for k, v := range a.Provider.Headers {
		req.Header.Set(k, v)
	}
	return s.opts.Client.Do(req)
}

func (s *Server) relay(w http.ResponseWriter, resp *http.Response, a router.Attempt) {
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	w.Header().Set(HeaderProvider, a.Provider.Name)
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// rejected reports whether a response means "try someone else": credentials,
// credits, rate limits or a server error.
func rejected(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	}
	return code >= 500
}

func (s *Server) cool(p *provider.Provider, resp *http.Response) {
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		p.Cool(time.Now().Add(s.opts.Cooldown), resp.Status)
	case http.StatusTooManyRequests:
		p.Cool(time.Now().Add(retryAfter(resp.Header.Get("Retry-After"))), resp.Status)
	}
}

func retryAfter(v string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && time.Until(t) > 0 {
		return time.Until(t)
	}
	return defaultBackoff
}

func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "gridtie_error", "code": code},
	})
}
