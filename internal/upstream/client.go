// Package upstream forwards OpenAI-compatible requests to a provider.
package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to an OpenAI-compatible chat/completions API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	userAgent  string
}

// New builds an upstream client.
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		userAgent: "modelrouter-gateway/1.0",
	}
}

// ProxyResult is the raw upstream HTTP response.
type ProxyResult struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

// ProxyChat posts a chat completion body and returns the upstream response.
// Caller must close Body.
func (c *Client) ProxyChat(ctx context.Context, body io.Reader, contentType string, stream bool) (*ProxyResult, error) {
	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", c.userAgent)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream chat: %w", err)
	}
	return &ProxyResult{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       resp.Body,
	}, nil
}

// HasAPIKey reports whether credentials are configured.
func (c *Client) HasAPIKey() bool { return c.apiKey != "" }

// BaseURL returns the configured upstream root.
func (c *Client) BaseURL() string { return c.baseURL }
