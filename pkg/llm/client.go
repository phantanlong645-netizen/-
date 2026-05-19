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

// CompleteChatMessages 发送聊天请求到LLM模型，获取完整的文本回复（非流式）
// ctx: 上下文对象，用于控制请求超时和取消
// messages: 对话消息历史，包括system、user、assistant角色的消息
// gen: 可选的生成参数，如果为nil则使用配置文件中的默认值
// 返回: 模型生成的文本内容，如果发生错误返回错误信息
func (c *deepseekClient) CompleteChatMessages(ctx context.Context, messages []Message, gen *GenerationParams) (string, error) {
	// 第一步：构建HTTP请求体
	reqBody := c.buildChatRequest(messages, gen, false)

	// 将请求体序列化为JSON字节数组
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal chat request: %w", err)
	}

	// 第二步：创建HTTP POST请求
	// 使用配置的基础URL + /chat/completions 端点
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(reqBytes))
	if err != nil {
		return "", fmt.Errorf("failed to create chat request: %w", err)
	}

	// 设置HTTP请求头
	req.Header.Set("Content-Type", "application/json")      // 请求体格式为JSON
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey) // Bearer Token认证

	// 第三步：发送HTTP请求到LLM服务
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call chat api: %w", err)
	}
	// 确保响应体被关闭，释放网络连接
	defer resp.Body.Close()

	// 第四步：检查HTTP响应状态码
	if resp.StatusCode != http.StatusOK {
		// 读取响应体用于错误诊断
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("chat api returned non-200 status: %s, body: %s", resp.Status, string(bodyBytes))
	}

	// 第五步：解析JSON响应
	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode chat response: %w", err)
	}

	// 第六步：提取生成的文本内容
	// 如果没有choices，说明模型没有生成任何内容
	if len(result.Choices) == 0 {
		return "", nil
	}
	// 返回第一个choice的消息内容
	return result.Choices[0].Message.Content, nil
}

// buildChatRequest 根据传入的消息列表和生成参数构建聊天请求体
// messages: 对话消息历史，包含角色和内容
// gen: 可选的生成参数控制，如果为nil则使用配置文件中的默认值
// stream: 是否启用流式响应
// 返回: 构建好的chatRequest请求体结构
func (c *deepseekClient) buildChatRequest(messages []Message, gen *GenerationParams, stream bool) chatRequest {
	// 初始化请求体，使用配置文件中的模型名称
	reqBody := chatRequest{
		Model:    c.cfg.Model,
		Messages: messages,
		Stream:   stream,
	}

	// 如果传入了生成参数，使用传入的值覆盖默认值
	if gen != nil {
		reqBody.Temperature = gen.Temperature
		reqBody.TopP = gen.TopP
		reqBody.MaxTokens = gen.MaxTokens
		return reqBody
	}

	// 使用配置文件中的默认值（如果配置了的话）
	// Temperature: 控制生成的随机性，0表示贪婪采样，较高的值增加随机性
	if c.cfg.Generation.Temperature != 0 {
		t := c.cfg.Generation.Temperature
		reqBody.Temperature = &t
	}
	// TopP: 核采样参数，控制候选词的多样性
	if c.cfg.Generation.TopP != 0 {
		p := c.cfg.Generation.TopP
		reqBody.TopP = &p
	}
	// MaxTokens: 生成内容的最大token数限制
	if c.cfg.Generation.MaxTokens != 0 {
		m := c.cfg.Generation.MaxTokens
		reqBody.MaxTokens = &m
	}
	return reqBody
}

// StreamChatMessages 调用 OpenAI 兼容的 chat completions 流式接口，逐块接收LLM生成的文本
// ctx: 上下文对象，用于控制请求超时和取消
// messages: 对话消息历史
// gen: 可选的生成参数，如果为nil则使用配置文件中的默认值
// writer: 消息写入器，用于将流式内容写入WebSocket或HTTP响应
// 返回: 错误信息，如果成功则返回nil
func (c *deepseekClient) StreamChatMessages(ctx context.Context, messages []Message, gen *GenerationParams, writer MessageWriter) error {
	// 第一步：构建HTTP请求体（启用流式模式）
	reqBody := chatRequest{
		Model:    c.cfg.Model,
		Messages: messages,
		Stream:   true, // 启用流式响应
	}

	// 根据生成参数或配置文件设置温度、TopP、MaxTokens
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

	// 第二步：序列化请求体为JSON
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal chat request: %w", err)
	}

	// 第三步：创建HTTP POST请求
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.cfg.BaseURL+"/chat/completions",
		bytes.NewReader(reqBytes),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat request: %w", err)
	}

	// 第四步：设置HTTP请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream") // 声明接受SSE流式响应

	// 第五步：发送HTTP请求
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call chat api: %w", err)
	}
	defer resp.Body.Close()

	// 第六步：检查HTTP响应状态码
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("chat api returned non-200 status: %s, body: %s", resp.Status, string(bodyBytes))
	}

	// 第七步：创建缓冲读取器，逐行读取SSE流
	reader := bufio.NewReader(resp.Body)
	for {
		// 读取一行数据（以\n分隔）
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// 流正常结束
				break
			}
			return fmt.Errorf("failed to read from stream: %w", err)
		}

		// 处理 SSE 格式：data: 开头的数据行
		if strings.HasPrefix(line, "data: ") {
			// 去除 "data: " 前缀
			data := strings.TrimPrefix(line, "data: ")
			// 检查是否是结束标志
			if strings.TrimSpace(data) == "[DONE]" {
				break
			}

			// 第八步：解析JSON获取内容块
			var chunk chatResponse
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				// 跳过无法解析的行
				continue
			}

			// 第九步：将内容块写入WebSocket或HTTP响应
			if len(chunk.Choices) > 0 {
				// 提取增量内容（流式响应中使用Delta字段）
				content := chunk.Choices[0].Delta.Content
				if err := writer.WriteMessage(websocket.TextMessage, []byte(content)); err != nil {
					return fmt.Errorf("failed to write message to websocket: %w", err)
				}
			}
		}
	}

	return nil
}

// CompleteWithTools 调用 OpenAI 兼容的 chat completions 接口，支持 OpenAI function calling 工具调用
// 这是 Agent 系统的核心方法，让 LLM 能够根据对话上下文自主调用预定义的工具（如搜索论文、选择论文等）
// ctx: 上下文对象，用于控制请求超时和取消
// messages: 对话消息历史，包括之前的工具调用结果
// tools: LLM 可调用的工具定义列表，每个工具包含名称、描述和参数模式
// 返回: AgentResponse 包含 LLM 的文本回复、工具调用列表、停止原因和 token 消耗统计
func (c *deepseekClient) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*AgentResponse, error) {
	// 第一步：构建带工具定义的请求体
	// ToolChoice 设置为 "auto"，让模型自己决定是否调用工具以及调用哪个工具
	reqBody := toolChatRequest{
		Model:      c.cfg.Model,
		Messages:   messages,
		Stream:     false, // 非流式响应，因为需要返回工具调用结果
		Tools:      tools,
		ToolChoice: "auto",
	}
	// 根据配置文件设置生成参数
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

	// 第二步：序列化请求体为JSON
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal tool request: %w", err)
	}

	// 第三步：创建HTTP POST请求
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create tool request: %w", err)
	}
	// 设置HTTP请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	// 第四步：发送HTTP请求到LLM服务
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call tool api: %w", err)
	}
	defer resp.Body.Close()

	// 第五步：检查HTTP响应状态码
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tool api returned non-200 status: %s, body: %s", resp.Status, string(bodyBytes))
	}

	// 第六步：解析JSON响应
	var result toolChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode tool response: %w", err)
	}

	// 第七步：处理空响应的情况
	if len(result.Choices) == 0 {
		return &AgentResponse{StopReason: "stop"}, nil
	}

	// 第八步：提取响应内容并构建AgentResponse
	choice := result.Choices[0]
	return &AgentResponse{
		Content:    choice.Message.Content,              // 文本回复内容（当StopReason为"stop"时）
		ToolCalls:  choice.Message.ToolCalls,           // 工具调用列表（当StopReason为"tool_calls"时）
		StopReason: choice.FinishReason,                // 停止原因：stop表示正常结束，tool_calls表示需要调用工具
		Usage: TokenUsage{                               // Token消耗统计
			PromptTokens:     result.Usage.PromptTokens,     // 输入消耗的token数
			CompletionTokens: result.Usage.CompletionTokens, // 输出消耗的token数
			TotalTokens:      result.Usage.TotalTokens,      // 总消耗token数
		},
	}, nil
}
