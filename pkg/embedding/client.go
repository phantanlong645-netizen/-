package embedding

import (
	"RAG-repository/internal/config"
	"RAG-repository/pkg/log"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Client 定义 Embedding 客户端接口。
// pipeline 和 search service 只依赖这个接口，不直接依赖具体实现。
type Client interface {
	CreateEmbedding(ctx context.Context, text string) ([]float32, error)
}

// openAICompatibleClient 是 OpenAI 兼容协议的 Embedding 客户端实现。
// DashScope、OpenAI、很多本地网关都可以用类似协议调用。
type openAICompatibleClient struct {
	cfg    config.EmbeddingConfig
	client *http.Client
}

// NewClient 根据配置创建 Embedding 客户端。
func NewClient(cfg config.EmbeddingConfig) Client {
	return &openAICompatibleClient{
		cfg:    cfg,
		client: &http.Client{},
	}
}

// embeddingRequest 是发送给 Embedding API 的请求体。
type embeddingRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

// embeddingResponse 是 Embedding API 返回的数据结构。
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// CreateEmbedding 调用 OpenAI 兼容接口，把文本转换成向量。
func (c *openAICompatibleClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	log.Infof("[EmbeddingClient] start embedding, model: %s, input_len: %d", c.cfg.Model, len(text))

	reqBody := embeddingRequest{
		Model:      c.cfg.Model,
		Input:      []string{text},
		Dimensions: c.cfg.Dimensions,
	}

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embedding request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.cfg.BaseURL+"/embeddings",
		bytes.NewReader(reqBytes),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create embedding request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.client.Do(req)
	if err != nil {
		log.Errorf("[EmbeddingClient] call embedding api failed, error: %v", err)
		return nil, fmt.Errorf("failed to call embedding api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Errorf("[EmbeddingClient] embedding api returned non-200 status: %s", resp.Status)
		return nil, fmt.Errorf("embedding api returned non-200 status: %s", resp.Status)
	}

	var embeddingResp embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&embeddingResp); err != nil {
		log.Errorf("[EmbeddingClient] decode embedding response failed, error: %v", err)
		return nil, fmt.Errorf("failed to decode embedding response: %w", err)
	}

	if len(embeddingResp.Data) == 0 || len(embeddingResp.Data[0].Embedding) == 0 {
		log.Warnf("[EmbeddingClient] embedding api returned empty vector")
		return nil, fmt.Errorf("received empty embedding from api")
	}

	log.Infof("[EmbeddingClient] embedding success, dimensions: %d", len(embeddingResp.Data[0].Embedding))

	return embeddingResp.Data[0].Embedding, nil
}
