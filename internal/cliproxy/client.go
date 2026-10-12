package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Credential struct {
	ID          string         `json:"id"`
	AuthIndex   string         `json:"auth_index"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Provider    string         `json:"provider"`
	Priority    int            `json:"priority"`
	Disabled    bool           `json:"disabled"`
	Unavailable bool           `json:"unavailable"`
	RuntimeOnly bool           `json:"runtime_only"`
	AccountType string         `json:"account_type"`
	Account     string         `json:"account"`
	IDToken     map[string]any `json:"id_token"`
}

func (c Credential) ProviderName() string {
	if provider := strings.ToLower(strings.TrimSpace(c.Provider)); provider != "" {
		return provider
	}
	return strings.ToLower(strings.TrimSpace(c.Type))
}

func (c Credential) TargetName() string {
	if name := strings.TrimSpace(c.Name); name != "" {
		return name
	}
	return strings.TrimSpace(c.ID)
}

func (c Credential) CodexAccountID() string {
	if c.IDToken != nil {
		for _, key := range []string{"chatgpt_account_id", "account_id"} {
			if value, ok := c.IDToken[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return strings.TrimSpace(c.Account)
}

type APICallRequest struct {
	AuthIndex string            `json:"auth_index,omitempty"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Header    map[string]string `json:"header,omitempty"`
	Data      string            `json:"data,omitempty"`
}

type APICallResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       string              `json:"body"`
}

type HTTPError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.StatusCode, strings.TrimSpace(e.Body))
}

func IsStatus(err error, status int) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == status
}

type Client struct {
	baseURL       string
	managementKey string
	requestedAPI  string
	userAgent     string
	http          *http.Client

	mu          sync.Mutex
	resolvedAPI string
}

func New(baseURL, managementKey, apiVersion, userAgent string, timeout time.Duration) *Client {
	return &Client{
		baseURL:       strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		managementKey: strings.TrimSpace(managementKey),
		requestedAPI:  strings.ToLower(strings.TrimSpace(apiVersion)),
		userAgent:     strings.TrimSpace(userAgent),
		http:          &http.Client{Timeout: timeout},
	}
}

func (c *Client) ListCredentials(ctx context.Context) ([]Credential, error) {
	api, err := c.apiVersion(ctx)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Files []Credential `json:"files"`
	}
	if err := c.doJSON(ctx, http.MethodGet, c.path(api, "credentials"), nil, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Files, nil
}

func (c *Client) PatchPriority(ctx context.Context, credential Credential, priority int) error {
	api, err := c.apiVersion(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{"name": credential.TargetName(), "priority": priority}
	return c.doJSON(ctx, http.MethodPatch, c.path(api, "fields"), body, nil)
}

func (c *Client) RefreshCredential(ctx context.Context, credential Credential) error {
	api, err := c.apiVersion(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{"name": credential.TargetName()}
	return c.doJSON(ctx, http.MethodPost, c.path(api, "refresh"), body, nil)
}

func (c *Client) APICall(ctx context.Context, request APICallRequest) (APICallResponse, error) {
	api, err := c.apiVersion(ctx)
	if err != nil {
		return APICallResponse{}, err
	}
	var response APICallResponse
	if err := c.doJSON(ctx, http.MethodPost, c.path(api, "api-call"), request, &response); err != nil {
		return APICallResponse{}, err
	}
	return response, nil
}

func (c *Client) APIVersion(ctx context.Context) (string, error) {
	return c.apiVersion(ctx)
}

func (c *Client) apiVersion(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.resolvedAPI != "" {
		resolved := c.resolvedAPI
		c.mu.Unlock()
		return resolved, nil
	}
	c.mu.Unlock()

	requested := c.requestedAPI
	if requested == "v8" || requested == "v0" {
		c.mu.Lock()
		c.resolvedAPI = requested
		c.mu.Unlock()
		return requested, nil
	}

	var probe struct {
		Files []Credential `json:"files"`
	}
	err := c.doJSON(ctx, http.MethodGet, c.path("v8", "credentials"), nil, &probe)
	if err == nil {
		c.mu.Lock()
		c.resolvedAPI = "v8"
		c.mu.Unlock()
		return "v8", nil
	}
	if !IsStatus(err, http.StatusNotFound) && !IsStatus(err, http.StatusMethodNotAllowed) {
		return "", fmt.Errorf("probe v8 Management API: %w", err)
	}

	if err := c.doJSON(ctx, http.MethodGet, c.path("v0", "credentials"), nil, &probe); err != nil {
		return "", fmt.Errorf("probe v0 Management API after v8 was unavailable: %w", err)
	}
	c.mu.Lock()
	c.resolvedAPI = "v0"
	c.mu.Unlock()
	return "v0", nil
}

func (c *Client) path(api, kind string) string {
	if api == "v8" {
		switch kind {
		case "credentials":
			return "/v8/management/credentials"
		case "fields":
			return "/v8/management/credentials/fields"
		case "refresh":
			return "/v8/management/credentials/refresh"
		case "api-call":
			return "/v8/management/requests/api-call"
		}
	}
	switch kind {
	case "credentials":
		return "/v0/management/auth-files"
	case "fields":
		return "/v0/management/auth-files/fields"
	case "refresh":
		return "/v0/management/auth-files/refresh"
	case "api-call":
		return "/v0/management/api-call"
	default:
		panic("unknown Management API path kind: " + kind)
	}
}

func (c *Client) doJSON(ctx context.Context, method, path string, input any, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if c.managementKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.managementKey)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Method: method, URL: url, StatusCode: resp.StatusCode, Body: string(data)}
	}
	if output == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}
