package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeIdP stands in for both the GitHub Actions OIDC endpoint and Anthropic's
// token endpoint. Like the real ones, each identity token is single-use.
type fakeIdP struct {
	mu        sync.Mutex
	minted    int
	exchanged map[string]bool
	exchanges int
	lastBody  map[string]string
	expiresIn int
	reject    bool
}

func (f *fakeIdP) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/gha", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer req-token" {
			http.Error(w, "bad request token", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("audience") != "https://api.anthropic.com" {
			http.Error(w, "bad audience", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.minted++
		n := f.minted
		f.mu.Unlock()
		fmt.Fprintf(w, `{"value":"jwt-%d"}`, n)
	})
	mux.HandleFunc("/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		json.Unmarshal(raw, &body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastBody = body
		if f.reject || f.exchanged[body["assertion"]] {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"Authentication failed"}}`)
			return
		}
		f.exchanged[body["assertion"]] = true
		f.exchanges++
		fmt.Fprintf(w, `{"access_token":"sk-ant-oat01-%d","token_type":"Bearer","expires_in":%d,"scope":"workspace:developer"}`,
			f.exchanges, f.expiresIn)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestSource(t *testing.T, f *fakeIdP) (*FederatedTokenSource, *time.Time) {
	srv := f.server(t)
	cfg := FederationConfig{
		FederationRuleID: "fdrl_x",
		OrganizationID:   "org-uuid",
		ServiceAccountID: "svac_x",
		WorkspaceID:      "wrkspc_x",
		// The runner's request URL already has a query string.
		IdentityToken: GitHubActionsIdentityToken(srv.Client(), srv.URL+"/gha?api-version=2.0", "req-token", "https://api.anthropic.com"),
	}
	ts := NewFederatedTokenSource(cfg, srv.Client(), srv.URL+"/v1/oauth/token")
	now := time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC)
	ts.now = func() time.Time { return now }
	return ts, &now
}

func TestFederatedTokenSource_ExchangeCacheRefresh(t *testing.T) {
	f := &fakeIdP{exchanged: map[string]bool{}, expiresIn: 600}
	ts, now := newTestSource(t, f)
	ctx := context.Background()

	tok, err := ts.Token(ctx)
	if err != nil || tok != "sk-ant-oat01-1" {
		t.Fatalf("first token = %q, %v", tok, err)
	}
	want := map[string]string{
		"grant_type":         "urn:ietf:params:oauth:grant-type:jwt-bearer",
		"assertion":          "jwt-1",
		"federation_rule_id": "fdrl_x",
		"organization_id":    "org-uuid",
		"service_account_id": "svac_x",
		"workspace_id":       "wrkspc_x",
	}
	for k, v := range want {
		if f.lastBody[k] != v {
			t.Errorf("exchange body %s = %q, want %q", k, f.lastBody[k], v)
		}
	}

	// Cached until the refresh margin (2 min before expiry).
	*now = now.Add(7 * time.Minute)
	if tok, _ := ts.Token(ctx); tok != "sk-ant-oat01-1" || f.exchanges != 1 {
		t.Errorf("expected cached token, got %q after %d exchanges", tok, f.exchanges)
	}

	// Inside the margin: refresh with a freshly minted identity token, so
	// the single-use check passes.
	*now = now.Add(90 * time.Second)
	tok, err = ts.Token(ctx)
	if err != nil || tok != "sk-ant-oat01-2" {
		t.Fatalf("refreshed token = %q, %v", tok, err)
	}
	if f.minted != 2 || f.lastBody["assertion"] != "jwt-2" {
		t.Errorf("refresh should mint a new identity token: minted=%d assertion=%q", f.minted, f.lastBody["assertion"])
	}
}

func TestFederatedTokenSource_RejectedIsNotRetryable(t *testing.T) {
	f := &fakeIdP{exchanged: map[string]bool{}, expiresIn: 600, reject: true}
	ts, _ := newTestSource(t, f)
	_, err := ts.Token(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusUnauthorized {
		t.Fatalf("want 401 HTTPError, got %v", err)
	}
	if Retryable(err) {
		t.Error("a denied exchange must not be retried")
	}
}

func TestAnthropic_UsesBearerFromTokenSource(t *testing.T) {
	f := &fakeIdP{exchanged: map[string]bool{}, expiresIn: 600}
	ts, _ := newTestSource(t, f)

	var auth, apiKey string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, apiKey = r.Header.Get("Authorization"), r.Header.Get("x-api-key")
		io.WriteString(w, `{"model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{}}`)
	}))
	defer api.Close()

	p, err := NewProvider("anthropic", "", WithBaseURL(api.URL), WithTokenSource(ts))
	if err != nil {
		t.Fatalf("NewProvider without key but with token source: %v", err)
	}
	if _, err := p.Complete(context.Background(), Request{Model: "m", MaxTokens: 10,
		Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if auth != "Bearer sk-ant-oat01-1" || apiKey != "" {
		t.Errorf("Authorization=%q x-api-key=%q", auth, apiKey)
	}
}

func TestFederationFromEnv(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_IDENTITY_TOKEN_FILE", "ANTHROPIC_IDENTITY_TOKEN", "ANTHROPIC_WORKSPACE_ID"} {
		t.Setenv(k, "")
	}
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "fdrl_x")
	t.Setenv("ANTHROPIC_ORGANIZATION_ID", "org")
	t.Setenv("ANTHROPIC_SERVICE_ACCOUNT_ID", "svac_x")

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
	if _, ok := FederationFromEnv(http.DefaultClient); ok {
		t.Error("no identity token source: federation must not activate")
	}

	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://example.invalid/token?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "req")
	cfg, ok := FederationFromEnv(http.DefaultClient)
	if !ok || cfg.IdentitySource != "GitHub Actions OIDC" {
		t.Errorf("GitHub Actions source not selected: ok=%v src=%q", ok, cfg.IdentitySource)
	}

	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "")
	if _, ok := FederationFromEnv(http.DefaultClient); ok {
		t.Error("missing rule ID: federation must not activate")
	}
}
