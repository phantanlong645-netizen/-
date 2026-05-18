// Package lightrag 封装对 LightRAG REST API 的访问。
// LightRAG 是一个基于知识图谱的 RAG 服务，通过 HTTP 接口提供：
//   - 文档插入：/documents/text
//   - 知识库查询（仅返回上下文）：/query with only_need_context=true
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

// Client 是 LightRAG HTTP 客户端。
type Client struct {
	cfg        config.LightRAGConfig
	httpClient *http.Client
}

// NewClient 创建 LightRAG 客户端。
func NewClient(cfg config.LightRAGConfig) *Client {
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			// 插入文档时 LightRAG 异步处理，HTTP 请求本身很快完成。
			Timeout: 30 * time.Second,
		},
	}
}

// InsertText 把纯文本内容异步发送给 LightRAG 建立知识图谱。
// LightRAG 服务器会在后台对文本做实体抽取和关系建图，不阻塞入库流程。
func (c *Client) InsertText(ctx context.Context, text, description string) error {
	body := map[string]string{
		"text":        text,
		"description": description,
	}
	return c.post(ctx, "/documents/text", body, nil)
}

// queryRequest 是调用 /query 接口的请求体。
type queryRequest struct {
	Query           string `json:"query"`
	Mode            string `json:"mode"`
	OnlyNeedContext bool   `json:"only_need_context"`
}

// queryResponse 是 /query 接口在 only_need_context=true 时的响应结构。
// 此时 response 字段包含的是 LightRAG 从知识图谱中检索到的上下文文本。
type queryResponse struct {
	Response string `json:"response"`
	Status   string `json:"status,omitempty"`
}

// QueryContext 向 LightRAG 发起查询，只返回检索上下文，不让 LightRAG 自行生成答案。
// mode 推荐使用 "mix"（向量 + 图谱混合检索），也可以用 "local"/"global"/"hybrid"/"naive"。
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

// post 是通用的 HTTP POST 帮助函数。
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
