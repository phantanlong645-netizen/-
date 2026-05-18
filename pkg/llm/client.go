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
	CompleteChatMessages(ctx context.Context, messages []Message, gen *GenerationParams) (string, error)
	StreamChatMessages(ctx context.Context, messages []Message, gen *GenerationParams, writer MessageWriter) error
	StreamChat(ctx context.Context, prompt string, writer MessageWriter) error
	// CompleteWithTools 发送带工具定义的请求，支持 OpenAI function calling。
	CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*AgentResponse, error)
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
// Content 为空时不序列化，兼容 tool_calls 和 tool 角色消息。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolFunction 描述 LLM 可调用的函数签名。
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Tool 是传给 LLM 的可调用工具（OpenAI function calling 格式）。
type Tool struct {
	Type     string       `json:"type"` // 固定为 "function"
	Function ToolFunction `json:"function"`
}

// ToolCallFunction 是模型返回的工具调用中的函数信息。
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 编码的参数
}

// ToolCall 是模型返回的单次工具调用。
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // 固定为 "function"
	Function ToolCallFunction `json:"function"`
}

// TokenUsage 记录单次 LLM 调用消耗的 token 数量。
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// AgentResponse 是 CompleteWithTools 的返回结果。
type AgentResponse struct {
	Content    string     // 文本内容（StopReason=="stop" 时有值）
	ToolCalls  []ToolCall // 工具调用列表（StopReason=="tool_calls" 时有值）
	StopReason string     // "stop" 或 "tool_calls"
	Usage      TokenUsage // 本次调用消耗的 token 数量
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
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// toolChatRequest 是带工具定义的聊天请求体。
type toolChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
}

// toolChatResponse 是工具调用接口的响应结构。
type toolChatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string     `json:"role"`
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
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

func (c *deepseekClient) CompleteChatMessages(ctx context.Context, messages []Message, gen *GenerationParams) (string, error) {
	reqBody := c.buildChatRequest(messages, gen, false)

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(reqBytes))
	if err != nil {
		return "", fmt.Errorf("failed to create chat request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call chat api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("chat api returned non-200 status: %s, body: %s", resp.Status, string(bodyBytes))
	}

	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode chat response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", nil
	}
	return result.Choices[0].Message.Content, nil
}

func (c *deepseekClient) buildChatRequest(messages []Message, gen *GenerationParams, stream bool) chatRequest {
	reqBody := chatRequest{
		Model:    c.cfg.Model,
		Messages: messages,
		Stream:   stream,
	}

	if gen != nil {
		reqBody.Temperature = gen.Temperature
		reqBody.TopP = gen.TopP
		reqBody.MaxTokens = gen.MaxTokens
		return reqBody
	}

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
	return reqBody
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

// CompleteWithTools 调用 OpenAI 兼容的 chat completions 接口，支持 function calling。
func (c *deepseekClient) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*AgentResponse, error) {
	reqBody := toolChatRequest{
		Model:      c.cfg.Model,
		Messages:   messages,
		Stream:     false,
		Tools:      tools,
		ToolChoice: "auto",
	}
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

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal tool request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create tool request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call tool api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tool api returned non-200 status: %s, body: %s", resp.Status, string(bodyBytes))
	}

	var result toolChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode tool response: %w", err)
	}
	if len(result.Choices) == 0 {
		return &AgentResponse{StopReason: "stop"}, nil
	}

	choice := result.Choices[0]
	return &AgentResponse{
		Content:    choice.Message.Content,
		ToolCalls:  choice.Message.ToolCalls,
		StopReason: choice.FinishReason,
		Usage: TokenUsage{
			PromptTokens:     result.Usage.PromptTokens,
			CompletionTokens: result.Usage.CompletionTokens,
			TotalTokens:      result.Usage.TotalTokens,
		},
	}, nil
}
