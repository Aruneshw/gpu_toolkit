package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"meshcompute/internal/sse"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const Timeout = 2 * time.Minute

// StreamTimeout is a hard upper bound for one streamed generation. It is far
// longer than a normal interactive request but still prevents an unresponsive
// backend from holding a handler forever.
const StreamTimeout = 30 * time.Minute

const MaxPromptBytes = 1 << 20
const MaxResponseBytes = 8 << 20

type Request struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
	Context   int    `json:"context"`
	// Pool names the host pool to run on; empty takes turns over every
	// ready pool serving Model.
	Pool string `json:"pool,omitempty"`
}
type Response struct {
	Content         string  `json:"content"`
	TokensPerSecond float64 `json:"tokens_per_second"`
}

// Chunk is one streamed piece of a completion. A final chunk has Done set and
// carries the server's measured generation rate.
type Chunk struct {
	Content         string  `json:"content,omitempty"`
	Done            bool    `json:"done,omitempty"`
	TokensPerSecond float64 `json:"tokens_per_second,omitempty"`
}

type Llama struct {
	endpoint string
	client   *http.Client
	// stream has no overall timeout: a long generation is bounded by the
	// caller's context and the agent's per-write deadline instead.
	stream *http.Client
}

type Adapter interface {
	Health(context.Context) (Capabilities, error)
	Generate(context.Context, Request) (Response, error)
}

var _ Adapter = (*Llama)(nil)

type Capabilities struct {
	Models  []string `json:"models"`
	Context int      `json:"context"`
}

func New(endpoint string) (*Llama, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || ip == nil || !ip.IsLoopback() || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("endpoint must be an http loopback IP origin")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.ResponseHeaderTimeout = Timeout
	client := &http.Client{Timeout: Timeout, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects forbidden") }}
	stream := *client
	stream.Timeout = 0
	return &Llama{endpoint: strings.TrimRight(endpoint, "/"), client: client, stream: &stream}, nil
}
func (l *Llama) call(ctx context.Context, method, path string, body, result any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, l.endpoint+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err = io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > MaxResponseBytes {
		return errors.New("backend response exceeds limit")
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("llama-server %s returned HTTP %d", path, res.StatusCode)
	}
	return json.Unmarshal(data, result)
}
func (l *Llama) Health(ctx context.Context) (Capabilities, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var health struct {
		Status string `json:"status"`
	}
	if err := l.call(ctx, "GET", "/health", nil, &health); err != nil {
		return Capabilities{}, err
	}
	if health.Status != "ok" {
		return Capabilities{}, errors.New("llama-server not ready")
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := l.call(ctx, "GET", "/v1/models", nil, &models); err != nil {
		return Capabilities{}, err
	}
	var props struct {
		Settings struct {
			Context int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := l.call(ctx, "GET", "/props", nil, &props); err != nil {
		return Capabilities{}, err
	}
	c := Capabilities{Context: props.Settings.Context, Models: []string{}}
	for _, m := range models.Data {
		c.Models = append(c.Models, m.ID)
	}
	return c, nil
}

// ValidRequest reports whether a request is within the bounded input limits.
func ValidRequest(r Request) bool {
	return r.Model != "" && strings.TrimSpace(r.Prompt) != "" && len(r.Prompt) <= MaxPromptBytes && r.MaxTokens >= 1 && r.MaxTokens <= 32768 && r.Context >= 1 && r.Context <= 1048576 && r.MaxTokens < r.Context && len(r.Pool) <= 64 && !strings.ContainsAny(r.Pool, "\x00\r\n")
}

// admit validates the same invariants for streaming and non-streaming calls:
// single-model residency, verified context, and an exact prompt token budget.
func (l *Llama) admit(ctx context.Context, r Request) error {
	c, err := l.Health(ctx)
	if err != nil {
		return err
	}
	// Router availability does not prove model residency; require a direct server.
	if len(c.Models) != 1 || c.Models[0] != r.Model {
		return errors.New("requested alias does not match single running server model")
	}
	if c.Context <= 0 || r.Context > c.Context {
		return errors.New("requested context exceeds verified server slot context, or context unavailable")
	}
	var tokens struct {
		Tokens []json.RawMessage `json:"tokens"`
	}
	if err := l.call(ctx, "POST", "/tokenize", map[string]any{"content": r.Prompt, "add_special": true}, &tokens); err != nil {
		return err
	}
	if len(tokens.Tokens) == 0 || len(tokens.Tokens)+r.MaxTokens > r.Context {
		return errors.New("prompt plus output exceeds context budget, or tokenizer returned no tokens")
	}
	return nil
}

func (l *Llama) Generate(ctx context.Context, r Request) (Response, error) {
	if !ValidRequest(r) {
		return Response{}, errors.New("invalid model, prompt, output limit or context budget")
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	if err := l.admit(ctx, r); err != nil {
		return Response{}, err
	}
	var completion struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
		Timings   struct {
			Rate float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	err := l.call(ctx, "POST", "/completion", map[string]any{"prompt": r.Prompt, "n_predict": r.MaxTokens, "stream": false, "cache_prompt": false}, &completion)
	if err != nil {
		return Response{}, err
	}
	if completion.Truncated {
		return Response{}, errors.New("server truncated prompt")
	}
	if completion.Timings.Rate < 0 {
		return Response{}, errors.New("invalid server timing")
	}
	return Response{Content: completion.Content, TokensPerSecond: completion.Timings.Rate}, nil
}

// GenerateStream runs one completion and calls emit for every non-empty token
// chunk and exactly one final Done chunk. It performs the same admission checks
// as Generate, never retries, and aborts when emit fails or ctx is cancelled.
func (l *Llama) GenerateStream(ctx context.Context, r Request, emit func(Chunk) error) error {
	if emit == nil {
		return errors.New("stream consumer required")
	}
	if !ValidRequest(r) {
		return errors.New("invalid model, prompt, output limit or context budget")
	}
	admitCtx, cancel := context.WithTimeout(ctx, Timeout)
	err := l.admit(admitCtx, r)
	cancel()
	if err != nil {
		return err
	}
	ctx, cancel = context.WithTimeout(ctx, StreamTimeout)
	defer cancel()
	data, err := json.Marshal(map[string]any{"prompt": r.Prompt, "n_predict": r.MaxTokens, "stream": true, "cache_prompt": false})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpoint+"/completion", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	res, err := l.stream.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("llama-server /completion returned HTTP %d", res.StatusCode)
	}
	sawStop := false
	err = sse.Read(res.Body, func(_ string, data []byte) error {
		if string(data) == "[DONE]" {
			if !sawStop {
				return errors.New("backend stream ended without final chunk")
			}
			return sse.Done
		}
		var chunk struct {
			Content   string `json:"content"`
			Stop      bool   `json:"stop"`
			Truncated bool   `json:"truncated"`
			Timings   struct {
				Rate float64 `json:"predicted_per_second"`
			} `json:"timings"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return errors.New("invalid backend stream chunk")
		}
		if chunk.Truncated {
			return errors.New("server truncated prompt")
		}
		if chunk.Timings.Rate < 0 {
			return errors.New("invalid server timing")
		}
		if chunk.Content != "" {
			if err := emit(Chunk{Content: chunk.Content}); err != nil {
				return err
			}
		}
		if chunk.Stop {
			sawStop = true
			if err := emit(Chunk{Done: true, TokensPerSecond: chunk.Timings.Rate}); err != nil {
				return err
			}
			return sse.Done
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !sawStop {
		return errors.New("backend stream ended without final chunk")
	}
	return nil
}
