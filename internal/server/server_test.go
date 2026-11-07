package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tpural/gridtie/internal/config"
	"github.com/tpural/gridtie/internal/provider"
	"github.com/tpural/gridtie/internal/router"
)

// upstream is a fake OpenAI-compatible provider that can also act as llama-swap.
type upstream struct {
	*httptest.Server
	name string

	mu       sync.Mutex
	status   int
	running  map[string]string // nil means /running fails
	probes   int
	warmed   []string
	bodies   []map[string]any
	auth     []string
	leaked   bool
	release  chan struct{}
	streamed bool
}

func newUpstream(t *testing.T, name string) *upstream {
	u := &upstream{name: name, status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /running", func(w http.ResponseWriter, _ *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.probes++
		if u.running == nil {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		var list []map[string]string
		for m, s := range u.running {
			list = append(list, map[string]string{"model": m, "state": s})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"running": list})
	})
	mux.HandleFunc("GET /upstream/{model}/health", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.warmed = append(u.warmed, r.PathValue("model"))
		u.mu.Unlock()
	})
	mux.HandleFunc("POST /chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.bodies = append(u.bodies, body)
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.leaked = u.leaked || r.Header.Get("X-Gridtie-Route") != ""
		status, release, streamed := u.status, u.release, u.streamed
		u.mu.Unlock()
		if streamed {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			<-release
			_, _ = io.WriteString(w, "data: second\n\n")
			return
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"from":%q}`, u.name)
	})
	u.Server = httptest.NewServer(mux)
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

type harness struct {
	t         *testing.T
	api       *Server
	providers map[string]*provider.Provider
	local     *upstream
	cloud     *upstream
	wakes     chan struct{}
}

func newHarness(t *testing.T, extra string) *harness {
	t.Helper()
	t.Setenv("CLOUD_KEY", "secret")
	h := &harness{t: t, local: newUpstream(t, "local"), cloud: newUpstream(t, "cloud"), wakes: make(chan struct{}, 10)}
	wake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { h.wakes <- struct{}{} }))
	t.Cleanup(wake.Close)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
providers:
  local:
    baseURL: %[1]s
    readiness: {llamaSwap: %[1]s}
    wake: {url: %[2]s}
  cloud:
    baseURL: %[3]s
    apiKeyEnv: CLOUD_KEY
models:
  coder:
    name: Coder
    aliases: [coding]
    routes:
      - {provider: local, model: coder-local}
      - {provider: cloud, model: vendor/coder}
  private:
    routes:
      - {provider: local, model: private-local}
%[4]s`, h.local.URL, wake.URL, h.cloud.URL, extra)))
	if err != nil {
		t.Fatal(err)
	}
	h.providers = map[string]*provider.Provider{}
	for name, pc := range cfg.Providers {
		if h.providers[name], err = provider.New(name, pc, http.DefaultClient, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := router.New(cfg, h.providers)
	if err != nil {
		t.Fatal(err)
	}
	h.api = New(Options{
		Router: rt, Client: http.DefaultClient, Cooldown: cfg.Cooldown.Duration,
		WakeOnModels: cfg.WakeOnModels, Log: slog.New(slog.DiscardHandler),
	})
	return h
}

func (h *harness) chat(model string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"temperature":0.2}`))
	req.RemoteAddr = "10.0.0.7:5000"
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	return rec
}

func (h *harness) wokeTimes(want int) {
	h.t.Helper()
	for range want {
		select {
		case <-h.wakes:
		case <-time.After(time.Second):
			h.t.Fatalf("wake hook not called %d times", want)
		}
	}
	select {
	case <-h.wakes:
		h.t.Fatal("wake hook called too often")
	case <-time.After(50 * time.Millisecond):
	}
}

func assertFrom(t *testing.T, rec *httptest.ResponseRecorder, provider string) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Header().Get(HeaderProvider) != provider || !strings.Contains(rec.Body.String(), `"`+provider+`"`) {
		t.Fatalf("want answer from %s, got %d %s %q", provider, rec.Code, rec.Header().Get(HeaderProvider), rec.Body)
	}
}

func TestColdLocalGoesToCloudAndWakesOnce(t *testing.T) {
	h := newHarness(t, "")
	assertFrom(t, h.chat("coder"), "cloud")
	assertFrom(t, h.chat("coding"), "cloud")
	h.wokeTimes(1)
	if h.local.calls() != 0 {
		t.Fatal("cold local got a request")
	}
	if got := h.cloud.bodies[0]["model"]; got != "vendor/coder" {
		t.Fatalf("cloud model = %v", got)
	}
	if h.cloud.auth[0] != "Bearer secret" {
		t.Fatalf("cloud auth = %q", h.cloud.auth[0])
	}
}

func TestReadyLocalGoesLocal(t *testing.T) {
	h := newHarness(t, "")
	h.local.running = map[string]string{"coder-local": "ready"}
	assertFrom(t, h.chat("coder"), "local")
	if h.cloud.calls() != 0 {
		t.Fatal("cloud used while local was ready")
	}
}

func TestOtherModelLoadedWarmsRequestedOne(t *testing.T) {
	h := newHarness(t, "")
	h.local.running = map[string]string{"other": "ready"}
	assertFrom(t, h.chat("coder"), "cloud")
	deadline := time.Now().Add(time.Second)
	for {
		h.local.mu.Lock()
		warmed := append([]string(nil), h.local.warmed...)
		h.local.mu.Unlock()
		if len(warmed) == 1 && warmed[0] == "coder-local" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warmed = %v", warmed)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.wokeTimes(0)
}

func TestLocalOnlyWaitsForLocal(t *testing.T) {
	h := newHarness(t, `
rules:
  - {header: "User-Agent=^n8n", localOnly: true}
  - {cidr: 10.0.0.0/8, localOnly: false}`)
	assertFrom(t, h.chat("coder", "X-Gridtie-Route", "local-only"), "local")
	assertFrom(t, h.chat("coder", "User-Agent", "n8n/1.0"), "local")
	assertFrom(t, h.chat("coder"), "cloud")
	assertFrom(t, h.chat("private"), "local")
	if h.cloud.calls() != 1 {
		t.Fatalf("cloud calls = %d, want 1", h.cloud.calls())
	}
	if h.local.leaked {
		t.Fatal("gridtie header leaked")
	}
}

func TestCloudOutOfCreditsFallsBackToLocalAndCools(t *testing.T) {
	h := newHarness(t, "")
	h.cloud.status = http.StatusPaymentRequired
	assertFrom(t, h.chat("coder"), "local")
	assertFrom(t, h.chat("coder"), "local")
	if h.cloud.calls() != 1 {
		t.Fatalf("cloud calls = %d, want 1 (cooling after 402)", h.cloud.calls())
	}
	if cooling, why, until := h.providers["cloud"].Cooling(); !cooling || !strings.Contains(why, "402") || time.Until(until) < 9*time.Minute {
		t.Fatalf("cooling = %v %q %v", cooling, why, until)
	}
}

func TestRateLimitHonoursRetryAfter(t *testing.T) {
	if d := retryAfter("120"); d != 2*time.Minute {
		t.Fatalf("retryAfter(120) = %v", d)
	}
	if d := retryAfter(""); d != defaultBackoff {
		t.Fatalf("retryAfter() = %v", d)
	}
	h := newHarness(t, "")
	h.cloud.status = http.StatusTooManyRequests
	assertFrom(t, h.chat("coder"), "local")
	if cooling, _, until := h.providers["cloud"].Cooling(); !cooling || time.Until(until) > 2*time.Minute {
		t.Fatalf("cooling = %v until %v", cooling, until)
	}
}

func TestOnlyModelFieldChanges(t *testing.T) {
	h := newHarness(t, "")
	h.chat("coder")
	got := h.cloud.bodies[0]
	if got["temperature"] != 0.2 || got["tools"].([]any)[0].(map[string]any)["type"] != "function" ||
		got["messages"].([]any)[0].(map[string]any)["content"] != "hi" {
		t.Fatalf("body changed: %v", got)
	}
}

func TestStreams(t *testing.T) {
	h := newHarness(t, "")
	h.cloud.streamed, h.cloud.release = true, make(chan struct{})
	srv := httptest.NewServer(h.api)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"coder","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first event = %q, %v", line, err)
	}
	close(h.cloud.release)
}

func TestModels(t *testing.T) {
	h := newHarness(t, "")
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var list struct {
		Data []struct {
			ID      string   `json:"id"`
			Aliases []string `json:"aliases"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Data) != 2 || list.Data[0].ID != "coder" || list.Data[0].Aliases[0] != "coding" {
		t.Fatalf("models = %s", rec.Body)
	}
	if h.local.probes != 0 {
		t.Fatal("listing models probed a provider")
	}

	h = newHarness(t, "wakeOnModels: true")
	h.api.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	h.wokeTimes(1)
}

func TestUnknownModel(t *testing.T) {
	h := newHarness(t, "")
	if rec := h.chat("nope"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "does not exist") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}
