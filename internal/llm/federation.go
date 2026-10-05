package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Workload Identity Federation (WIF): exchange a short-lived identity token
// from an OIDC provider (here, usually GitHub Actions) for a short-lived
// Anthropic access token, instead of using a static API key.
//
// Docs: https://platform.claude.com/docs/en/manage-claude/workload-identity-federation

const (
	anthropicTokenURL   = "https://api.anthropic.com/v1/oauth/token"
	anthropicAudience   = "https://api.anthropic.com"
	jwtBearerGrant      = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	tokenRefreshMargin  = 2 * time.Minute
	defaultTokenTimeout = 30 * time.Second
)

// TokenSource yields a bearer token for API requests.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// IdentityTokenFunc returns a fresh identity token (JWT) from the workload's
// identity provider. It is called once per exchange: GitHub tokens carry a
// jti and can only be exchanged once.
type IdentityTokenFunc func(ctx context.Context) (string, error)

// FederationConfig identifies the federation rule and target of an exchange.
type FederationConfig struct {
	FederationRuleID string
	OrganizationID   string
	ServiceAccountID string
	WorkspaceID      string // optional when the rule covers one workspace
	IdentityToken    IdentityTokenFunc
	// IdentitySource describes where identity tokens come from, for logs.
	IdentitySource string
}

// FederationFromEnv reads the standard Anthropic SDK federation variables:
// ANTHROPIC_FEDERATION_RULE_ID, ANTHROPIC_ORGANIZATION_ID,
// ANTHROPIC_SERVICE_ACCOUNT_ID, optional ANTHROPIC_WORKSPACE_ID, and an
// identity token from ANTHROPIC_IDENTITY_TOKEN_FILE, ANTHROPIC_IDENTITY_TOKEN,
// or — inside a GitHub Actions job with `id-token: write` — the runner's
// token endpoint, which mints a fresh token per exchange. ok is false when
// federation isn't configured.
func FederationFromEnv(client *http.Client) (cfg FederationConfig, ok bool) {
	cfg = FederationConfig{
		FederationRuleID: os.Getenv("ANTHROPIC_FEDERATION_RULE_ID"),
		OrganizationID:   os.Getenv("ANTHROPIC_ORGANIZATION_ID"),
		ServiceAccountID: os.Getenv("ANTHROPIC_SERVICE_ACCOUNT_ID"),
		WorkspaceID:      os.Getenv("ANTHROPIC_WORKSPACE_ID"),
	}
	if cfg.FederationRuleID == "" || cfg.OrganizationID == "" || cfg.ServiceAccountID == "" {
		return cfg, false
	}
	switch {
	case os.Getenv("ANTHROPIC_IDENTITY_TOKEN_FILE") != "":
		path := os.Getenv("ANTHROPIC_IDENTITY_TOKEN_FILE")
		cfg.IdentityToken = identityTokenFromFile(path)
		cfg.IdentitySource = "file " + path
	case os.Getenv("ANTHROPIC_IDENTITY_TOKEN") != "":
		tok := strings.TrimSpace(os.Getenv("ANTHROPIC_IDENTITY_TOKEN"))
		cfg.IdentityToken = func(context.Context) (string, error) { return tok, nil }
		cfg.IdentitySource = "ANTHROPIC_IDENTITY_TOKEN"
	case os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL") != "" && os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN") != "":
		cfg.IdentityToken = GitHubActionsIdentityToken(client,
			os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"),
			os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"),
			anthropicAudience)
		cfg.IdentitySource = "GitHub Actions OIDC"
	default:
		return cfg, false
	}
	return cfg, true
}

func identityTokenFromFile(path string) IdentityTokenFunc {
	return func(context.Context) (string, error) {
		// Re-read on every exchange so a rotated token is picked up.
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read identity token: %w", err)
		}
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("identity token file %s is empty", path)
		}
		return tok, nil
	}
}

// GitHubActionsIdentityToken requests a fresh OIDC token from the Actions
// runner. requestURL already carries a query string, so the audience is
// appended with '&'.
func GitHubActionsIdentityToken(client *http.Client, requestURL, requestToken, audience string) IdentityTokenFunc {
	return func(ctx context.Context) (string, error) {
		sep := "&"
		if !strings.Contains(requestURL, "?") {
			sep = "?"
		}
		u := requestURL + sep + "audience=" + url.QueryEscape(audience)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+requestToken)
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("github oidc: %w", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("github oidc: http %d: %s", resp.StatusCode, truncate(string(raw), 200))
		}
		var out struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.Value == "" {
			return "", fmt.Errorf("github oidc: no token in response")
		}
		return out.Value, nil
	}
}

// FederatedTokenSource exchanges identity tokens for Anthropic access tokens,
// caching each until shortly before it expires. Safe for concurrent use;
// concurrent callers share one exchange.
type FederatedTokenSource struct {
	cfg      FederationConfig
	client   *http.Client
	tokenURL string
	now      func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewFederatedTokenSource builds a token source. tokenURL may be empty for
// the production endpoint.
func NewFederatedTokenSource(cfg FederationConfig, client *http.Client, tokenURL string) *FederatedTokenSource {
	if client == nil {
		client = &http.Client{Timeout: defaultTokenTimeout}
	}
	if tokenURL == "" {
		tokenURL = anthropicTokenURL
	}
	return &FederatedTokenSource{cfg: cfg, client: client, tokenURL: tokenURL, now: time.Now}
}

// Token implements TokenSource.
func (s *FederatedTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Before(s.expires.Add(-tokenRefreshMargin)) {
		return s.token, nil
	}
	tok, ttl, err := s.exchange(ctx)
	if err != nil {
		return "", err
	}
	s.token = tok
	s.expires = s.now().Add(ttl)
	return s.token, nil
}

func (s *FederatedTokenSource) exchange(ctx context.Context) (string, time.Duration, error) {
	if s.cfg.IdentityToken == nil {
		return "", 0, errors.New("federation: no identity token source")
	}
	assertion, err := s.cfg.IdentityToken(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("federation: %w", err)
	}
	body := map[string]string{
		"grant_type":         jwtBearerGrant,
		"assertion":          assertion,
		"federation_rule_id": s.cfg.FederationRuleID,
		"organization_id":    s.cfg.OrganizationID,
		"service_account_id": s.cfg.ServiceAccountID,
	}
	if s.cfg.WorkspaceID != "" {
		body["workspace_id"] = s.cfg.WorkspaceID
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, bytes.NewReader(payload))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("federation exchange: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 400 {
		// HTTPError keeps the retry policy: 5xx/429 retry, 401 does not.
		// A 401 is deliberately opaque; the deny reason is on the Console's
		// Workload identity → History page.
		return "", 0, &HTTPError{Provider: "anthropic-oauth", Status: resp.StatusCode, Body: string(raw)}
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" {
		return "", 0, errors.New("federation exchange: response has no access_token")
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Minute
	}
	return out.AccessToken, ttl, nil
}
