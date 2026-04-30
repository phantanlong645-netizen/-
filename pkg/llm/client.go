package llm

import (
	"RAG-repository/internal/config"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// MessageWriter 抽象 WebSocket 写入能力。
// 这样 LLM 客户端不用直接绑定 websocket.Conn，测试和扩展更方便。
type MessageWriter interface {
	WriteMessage(messageType int, data []byte) error
}

// Client 定义 LLM 客户端接口。
// 后面的 chat service 只依赖接口，不依赖具体模型厂商。
type Client interface {
	StreamChatMessages(ctx context.Context, messages []Message, gen *GenerationParams, writer MessageWriter) error
	StreamChat(ctx context.Context, prompt string, writer MessageWriter) error
}

// deepseekClient 是 OpenAI 兼容聊天接口的实现。
// 当前配置默认指向 DeepSeek，但也可以切到本地 Ollama 等兼容接口。
type deepseekClient struct {
	cfg    config.LLMConfig
	client *http.Client
}

// NewClient 创建 LLM 客户端。
func NewClient(cfg config.LLMConfig) Client {
	return &deepseekClient{
		cfg:    cfg,
		client: &http.Client{},
	}
}

// Message 表示一条 role-based 聊天消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest 是发送给聊天模型的请求体。
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
}

// chatResponse 是流式接口每个 chunk 的响应结构。
type chatResponse struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// GenerationParams 控制模型生成参数。
type GenerationParams struct {
	Temperature *float64
	TopP        *float64
	MaxTokens   *int
}

// StreamChat 是兼容旧调用的简化方法。
// 内部会把 prompt 包装成一条 user 消息。
func (c *deepseekClient) StreamChat(ctx context.Context, prompt string, writer MessageWriter) error {
	return c.StreamChatMessages(ctx, []Message{{Role: "user", Content: prompt}}, nil, writer)
}

// StreamChatMessages 调用 OpenAI 兼容的 chat completions 流式接口。
func (c *deepseekClient) StreamChatMessages(ctx context.Context, messages []Message, gen *GenerationParams, writer MessageWriter) error {
	reqBody := chatRequest{
		Model:    c.cfg.Model,
		Messages: messages,
		Stream:   true,
	}

	if gen != nil {
		reqBody.Temperature = gen.Temperature
		reqBody.TopP = gen.TopP
		reqBody.MaxTokens = gen.MaxTokens
	} else {
		if c.cfg.Generation.Temperature != 0 {
			t := c.cfg.Generation.Temperature
			reqBody.Temperature = &t
		}
		if c.cfg.Generation.TopP != 0 {
			p := c.cfg.Generation.TopP
			reqBody.TopP = &p
		}
		if c.cfg.Generation.MaxTokens != 0 {
			m := c.cfg.Generation.MaxTokens
			reqBody.MaxTokens = &m
		}
	}

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.cfg.BaseURL+"/chat/completions",
		bytes.NewReader(reqBytes),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call chat api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("chat api returned non-200 status: %s, body: %s", resp.Status, string(bodyBytes))
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("failed to read from stream: %w", err)
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if strings.TrimSpace(data) == "[DONE]" {
				break
			}

			var chunk chatResponse
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			if len(chunk.Choices) > 0 {
				content := chunk.Choices[0].Delta.Content
				if err := writer.WriteMessage(websocket.TextMessage, []byte(content)); err != nil {
					return fmt.Errorf("failed to write message to websocket: %w", err)
				}
			}
		}
	}

	return nil
}
