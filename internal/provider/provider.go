// Package provider defines the upstream adapter contract. Every adapter
// streams from its upstream (even for non-streaming clients) and emits
// canonical events, so the dispatcher has one code path, always measures
// time-to-first-token, and can fail over until the first visible token.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
)

// Call is everything an adapter needs for one upstream attempt.
type Call struct {
	Req        *canon.Request
	Model      *catalog.Model // nil for uncatalogued passthrough
	UpstreamID string
	Effort     string // reasoning level; "" = provider default
	MaxTokens  int    // wire max output tokens (always > 0)
	CacheHint  bool   // conversation is multi-turn: enable prompt-cache markers
	SessionKey string // stable conversation key (OpenAI prompt_cache_key)
	RequestID  string
}

// Caps returns model capabilities (permissive for passthrough models).
func (c *Call) Caps() catalog.Caps {
	if c.Model != nil {
		return c.Model.Caps
	}
	return catalog.Caps{Tools: true, ForcedToolChoice: true, Vision: true, ImageURLs: true, PDF: true, JSONSchema: true, Sampling: true, Prefill: true, Seed: true}
}

// ThinkingStyle returns how effort is expressed for this call.
func (c *Call) ThinkingStyle() string {
	if c.Model != nil {
		return c.Model.ThinkingStyle
	}
	return catalog.ThinkNone
}

// Stream yields canonical events until io.EOF.
type Stream interface {
	Next() (canon.Event, error)
	Header() http.Header
	Close() error
}

// Provider is one upstream API.
type Provider interface {
	Name() string
	Open(ctx context.Context, call *Call) (Stream, error)
}

// Kind classifies failures for retry/health decisions.
type Kind uint8

const (
	KindTransport  Kind = iota + 1 // connect/reset/EOF: retry, counts against health
	KindTimeout                    // first-token or idle timeout: retry, counts against health
	KindRateLimit                  // 429: retry elsewhere, cooldown (not a health failure)
	KindServer                     // 5xx/overloaded: retry, counts against health
	KindBadRequest                 // 400/422: retry once on a different provider
	KindAuth                       // 401/403: provider misconfigured
	KindNotFound                   // 404: model id unknown upstream
	KindCanceled                   // client went away: stop, no health impact
	KindBadOutput                  // upstream returned unusable output (empty/malformed)
)

func (k Kind) String() string {
	switch k {
	case KindTransport:
		return "transport"
	case KindTimeout:
		return "timeout"
	case KindRateLimit:
		return "rate_limit"
	case KindServer:
		return "server"
	case KindBadRequest:
		return "bad_request"
	case KindAuth:
		return "auth"
	case KindNotFound:
		return "not_found"
	case KindCanceled:
		return "canceled"
	case KindBadOutput:
		return "bad_output"
	}
	return "unknown"
}

// Retryable reports whether another candidate may succeed.
func (k Kind) Retryable() bool { return k != KindCanceled }

// HealthFailure reports whether the failure indicates an unhealthy model.
func (k Kind) HealthFailure() bool {
	return k == KindTransport || k == KindTimeout || k == KindServer
}

// Error is a classified upstream failure.
type Error struct {
	Provider   string
	Model      string
	Kind       Kind
	Status     int
	Message    string
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s %s: %s", e.Provider, e.Model, e.Kind)
	if e.Status > 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// AsError extracts or classifies an error.
func AsError(err error, prov, model string) *Error {
	if err == nil {
		return nil
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return Classify(err, prov, model)
}

// ErrFirstTokenTimeout and ErrIdleTimeout are context cancellation causes.
var (
	ErrFirstTokenTimeout = errors.New("first token timeout")
	ErrIdleTimeout       = errors.New("stream idle timeout")
	ErrClientGone        = errors.New("client disconnected")
)

// Classify maps transport errors to a Kind.
func Classify(err error, prov, model string) *Error {
	e := &Error{Provider: prov, Model: model, Kind: KindTransport, Message: err.Error(), Err: err}
	switch {
	case errors.Is(err, ErrClientGone):
		e.Kind = KindCanceled
	case errors.Is(err, ErrFirstTokenTimeout), errors.Is(err, ErrIdleTimeout), errors.Is(err, context.DeadlineExceeded):
		e.Kind = KindTimeout
	case errors.Is(err, context.Canceled):
		e.Kind = KindCanceled
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			e.Kind = KindTimeout
		}
	}
	return e
}

// FromHTTP builds an Error from a non-2xx upstream response, reading a
// bounded amount of the body for the message.
func FromHTTP(prov, model string, resp *http.Response) *Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	e := &Error{Provider: prov, Model: model, Status: resp.StatusCode, Message: errorMessage(body)}
	switch s := resp.StatusCode; {
	case s == 429:
		e.Kind = KindRateLimit
	case s == 401 || s == 403:
		e.Kind = KindAuth
	case s == 404:
		e.Kind = KindNotFound
	case s == 400 || s == 413 || s == 422:
		e.Kind = KindBadRequest
	case s == 408:
		e.Kind = KindTimeout
	case s >= 500:
		e.Kind = KindServer
	default:
		e.Kind = KindBadRequest
	}
	e.RetryAfter = RetryAfter(resp.Header, body)
	return e
}

// RetryAfter parses Retry-After / retry-after-ms headers or a Gemini-style
// retryDelay in the body.
func RetryAfter(h http.Header, body []byte) time.Duration {
	if v := h.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	if v := h.Get("Retry-After"); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil {
			return time.Duration(s * float64(time.Second))
		}
		if t, err := http.ParseTime(v); err == nil {
			return time.Until(t)
		}
	}
	if i := strings.Index(string(body), `"retryDelay"`); i >= 0 {
		rest := string(body[i+len(`"retryDelay"`):])
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			rest = rest[j+1:]
			if k := strings.IndexByte(rest, '"'); k > 0 {
				if d, err := time.ParseDuration(rest[:k]); err == nil {
					return d
				}
			}
		}
	}
	return 0
}

func errorMessage(body []byte) string {
	var env struct {
		Error json.RawMessage `json:"error"`
		Msg   string          `json:"message"`
	}
	if json.Unmarshal(body, &env) == nil {
		if len(env.Error) > 0 {
			var inner struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Status  string `json:"status"`
			}
			if json.Unmarshal(env.Error, &inner) == nil && inner.Message != "" {
				t := inner.Type
				if t == "" {
					t = inner.Status
				}
				if t != "" {
					return t + ": " + inner.Message
				}
				return inner.Message
			}
			var s string
			if json.Unmarshal(env.Error, &s) == nil {
				return s
			}
		}
		if env.Msg != "" {
			return env.Msg
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// NewHTTPClient returns a pooled HTTP/2-capable client without an overall
// timeout (streams are bounded by the dispatcher's first-token, idle, and
// total timers).
func NewHTTPClient(connectTimeout time.Duration) *http.Client {
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   connectTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: tr}
}

// ParseArgs parses tool-call argument JSON into an object (invalid or empty → {}).
func ParseArgs(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	var v map[string]any
	if json.Unmarshal([]byte(s), &v) != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(s)
}

// DataURL splits a data: URL into media type and base64 payload.
func DataURL(u string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	rest := u[5:]
	i := strings.IndexByte(rest, ',')
	if i < 0 {
		return "", "", false
	}
	meta, data := rest[:i], rest[i+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	return strings.TrimSuffix(meta, ";base64"), data, true
}
