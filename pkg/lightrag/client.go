// Package lightrag wraps access to the LightRAG REST API.
// LightRAG provides:
//   - document ingestion: /documents/text
//   - context-only query: /query with only_need_context=true
package lightrag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"RAG-repository/internal/config"
	"RAG-repository/pkg/log"
)

const lightragRequestTimeout = 30 * time.Second

// Client is a LightRAG HTTP client.
type Client struct {
	cfg        config.LightRAGConfig
	httpClient *http.Client
}

// NewClient creates a LightRAG client.
func NewClient(cfg config.LightRAGConfig) *Client {
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: lightragRequestTimeout,
		},
	}
}

// InsertText sends plain text to LightRAG for knowledge graph building.
func (c *Client) InsertText(ctx context.Context, text, description string) error {
	body := map[string]string{
		"text":        text,
		"description": description,
	}
	return c.post(ctx, "/documents/text", body, nil)
}

type queryRequest struct {
	Query           string `json:"query"`
	Mode            string `json:"mode"`
	OnlyNeedContext bool   `json:"only_need_context"`
}

type queryResponse struct {
	Response string `json:"response"`
	Status   string `json:"status,omitempty"`
}

// QueryContext asks LightRAG for context only.
func (c *Client) QueryContext(ctx context.Context, query string, mode string) (string, error) {
	if mode == "" {
		mode = "mix"
	}
	req := queryRequest{
		Query:           query,
		Mode:            mode,
		OnlyNeedContext: true,
	}
	var resp queryResponse
	if err := c.post(ctx, "/query", req, &resp); err != nil {
		return "", err
	}
	return resp.Response, nil
}

func (c *Client) post(ctx context.Context, path string, body interface{}, out interface{}) error {
	reqBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("lightrag: marshal request: %w", err)
	}

	url := c.cfg.URL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBytes))
	if err != nil {
		return fmt.Errorf("lightrag: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", c.cfg.APIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("lightrag: http %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("lightrag: %s returned %d: %s", path, resp.StatusCode, string(raw))
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("lightrag: decode response: %w", err)
		}
	}

	log.Infof("[LightRAG] POST %s OK", path)
	return nil
}
