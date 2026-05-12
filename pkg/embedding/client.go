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
	// 记录本次向量化开始；这里的 len(text) 是字节长度，不是 token 数。
	log.Infof("[EmbeddingClient] start embedding, model: %s, input_len: %d", c.cfg.Model, len(text))

	// 构造 OpenAI 兼容 embedding 接口需要的请求体。
	reqBody := embeddingRequest{
		// 使用配置里的 embedding 模型名。
		Model: c.cfg.Model,
		// 接口支持批量 input，这里一次只传一个文本块。
		Input: []string{text},
		// 指定期望向量维度；如果为 0，omitempty 会让它不出现在 JSON 里。
		Dimensions: c.cfg.Dimensions,
	}

	// 把请求体结构体序列化成 JSON 字节。
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		// JSON 序列化失败时，说明请求体还没发出去，直接返回错误。
		return nil, fmt.Errorf("failed to marshal embedding request: %w", err)
	}

	// 创建一个带 context 的 HTTP POST 请求，方便上层取消或超时控制。
	req, err := http.NewRequestWithContext(
		// 调用方传入的 context。
		ctx,
		// embedding 接口使用 POST。
		http.MethodPost,
		// OpenAI 兼容接口路径：base_url + /embeddings。
		c.cfg.BaseURL+"/embeddings",
		// 请求 body 是刚才序列化出的 JSON。
		bytes.NewReader(reqBytes),
	)
	if err != nil {
		// URL 非法或 request 创建失败时返回错误。
		return nil, fmt.Errorf("failed to create embedding request: %w", err)
	}

	// 声明请求体格式是 JSON。
	req.Header.Set("Content-Type", "application/json")
	// 使用 Bearer Token 方式携带 API Key。
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	// 真正发起 HTTP 请求，调用 embedding 服务。
	resp, err := c.client.Do(req)
	if err != nil {
		// 网络错误、超时、DNS 错误等会进入这里。
		log.Errorf("[EmbeddingClient] call embedding api failed, error: %v", err)
		return nil, fmt.Errorf("failed to call embedding api: %w", err)
	}
	// 读取完成或提前返回时关闭响应体，避免连接泄漏。
	defer resp.Body.Close()

	// 只接受 200 OK，其他状态都认为接口调用失败。
	if resp.StatusCode != http.StatusOK {
		log.Errorf("[EmbeddingClient] embedding api returned non-200 status: %s", resp.Status)
		return nil, fmt.Errorf("embedding api returned non-200 status: %s", resp.Status)
	}

	// 准备用来承接 embedding API 返回 JSON 的结构体。
	var embeddingResp embeddingResponse
	// 解码响应 JSON，拿到 data[0].embedding。
	if err := json.NewDecoder(resp.Body).Decode(&embeddingResp); err != nil {
		log.Errorf("[EmbeddingClient] decode embedding response failed, error: %v", err)
		return nil, fmt.Errorf("failed to decode embedding response: %w", err)
	}

	// 防御性校验：必须至少返回一条 data，且第一条 embedding 不能为空。
	if len(embeddingResp.Data) == 0 || len(embeddingResp.Data[0].Embedding) == 0 {
		log.Warnf("[EmbeddingClient] embedding api returned empty vector")
		return nil, fmt.Errorf("received empty embedding from api")
	}

	// 记录向量化成功，以及实际返回的向量维度。
	log.Infof("[EmbeddingClient] embedding success, dimensions: %d", len(embeddingResp.Data[0].Embedding))

	// 返回当前文本块对应的向量。
	return embeddingResp.Data[0].Embedding, nil
}
