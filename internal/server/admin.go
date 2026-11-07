package server

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tpural/gridtie/internal/provider"
	"github.com/tpural/gridtie/internal/router"
)

// Metrics are the Prometheus metrics gridtie exports.
type Metrics struct {
	requests *prometheus.CounterVec
	skips    *prometheus.CounterVec
	seconds  *prometheus.HistogramVec
}

// NewMetrics registers the metrics with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gridtie_requests_total", Help: "Requests by model, the provider that answered, and status code.",
		}, []string{"model", "provider", "code"}),
		skips: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gridtie_skips_total", Help: "Provider attempts that failed over to the next route, by reason.",
		}, []string{"provider", "reason"}),
		seconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gridtie_request_seconds", Help: "Time from request to the end of the response.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 12),
		}, []string{"provider"}),
	}
	reg.MustRegister(m.requests, m.skips, m.seconds)
	return m
}

// Admin serves health, metrics and status.
func Admin(r *router.Router, providers map[string]*provider.Provider, gatherer prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, req *http.Request) {
		type prov struct {
			Name         string    `json:"name"`
			Cooling      bool      `json:"cooling"`
			CoolingWhy   string    `json:"coolingWhy,omitempty"`
			CoolingUntil time.Time `json:"coolingUntil,omitzero"`
		}
		type model struct {
			ID        string         `json:"id"`
			Preferred string         `json:"preferred"`
			State     provider.State `json:"state"`
		}
		var out struct {
			Providers []prov  `json:"providers"`
			Models    []model `json:"models"`
		}
		for _, name := range slices.Sorted(maps.Keys(providers)) {
			c, why, until := providers[name].Cooling()
			out.Providers = append(out.Providers, prov{Name: name, Cooling: c, CoolingWhy: why, CoolingUntil: until})
		}
		for _, m := range r.Models() {
			first := m.Routes[0]
			out.Models = append(out.Models, model{ID: m.ID, Preferred: first.Provider, State: providers[first.Provider].State(req.Context(), first.Model)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	return mux
}
