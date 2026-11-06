package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const Timeout = 2 * time.Minute
const MaxPromptBytes = 1 << 20
const MaxResponseBytes = 8 << 20

type Request struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
	Context   int    `json:"context"`
}
type Response struct {
	Content         string  `json:"content"`
	TokensPerSecond float64 `json:"tokens_per_second"`
}
type Llama struct {
	endpoint string
	client   *http.Client
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
	return &Llama{strings.TrimRight(endpoint, "/"), &http.Client{Timeout: Timeout, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects forbidden") }}}, nil
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
func (l *Llama) Generate(ctx context.Context, r Request) (Response, error) {
	if r.Model == "" || strings.TrimSpace(r.Prompt) == "" || len(r.Prompt) > MaxPromptBytes || r.MaxTokens < 1 || r.MaxTokens > 32768 || r.Context < 1 || r.Context > 1048576 || r.MaxTokens >= r.Context {
		return Response{}, errors.New("invalid model, prompt, output limit or context budget")
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	c, err := l.Health(ctx)
	if err != nil {
		return Response{}, err
	}
	// Router availability does not prove model residency; require a direct server.
	if len(c.Models) != 1 || c.Models[0] != r.Model {
		return Response{}, errors.New("requested alias does not match single running server model")
	}
	if c.Context <= 0 || r.Context > c.Context {
		return Response{}, errors.New("requested context exceeds verified server slot context, or context unavailable")
	}
	var tokens struct {
		Tokens []json.RawMessage `json:"tokens"`
	}
	if err := l.call(ctx, "POST", "/tokenize", map[string]any{"content": r.Prompt, "add_special": true}, &tokens); err != nil {
		return Response{}, err
	}
	if len(tokens.Tokens) == 0 || len(tokens.Tokens)+r.MaxTokens > r.Context {
		return Response{}, errors.New("prompt plus output exceeds context budget, or tokenizer returned no tokens")
	}
	var completion struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
		Timings   struct {
			Rate float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	err = l.call(ctx, "POST", "/completion", map[string]any{"prompt": r.Prompt, "n_predict": r.MaxTokens, "stream": false, "cache_prompt": false}, &completion)
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
