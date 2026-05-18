// Package service 放业务层代码。
package service

import (
	// context 用来控制一次聊天请求的生命周期，比如客户端断开后取消请求。
	"context"
	// encoding/json 用来把 WebSocket 返回数据包装成 JSON。
	"encoding/json"
	// fmt 用来构造错误信息和格式化上下文文本。
	"fmt"
	// strings 用来高效拼接 prompt、上下文和流式回答。
	"strings"
	// sync 用来协调并行检索的 goroutine。
	"sync"
	// time 用来记录聊天消息时间，以及发送完成通知时间。
	"time"

	// config 读取 LLM prompt、生成参数等配置。
	"RAG-repository/internal/config"
	// model 放用户、聊天消息、搜索结果 DTO 等业务模型。
	"RAG-repository/internal/model"
	// repository 提供会话历史的 Redis 读写能力。
	"RAG-repository/internal/repository"
	// lightrag 是知识图谱检索服务客户端。
	"RAG-repository/pkg/lightrag"
	// llm 是大模型客户端抽象，ChatService 通过它调用模型。
	"RAG-repository/pkg/llm"
	// log 记录业务日志。
	"RAG-repository/pkg/log"

	// websocket 是聊天接口使用的长连接协议。
	"github.com/gorilla/websocket"
)

// intentResult 是意图分析阶段的结构化输出。
type intentResult struct {
	// Intent 是对用户问题意图的一句话概括。
	Intent string `json:"intent"`
	// Steps 是解决该问题的思维步骤列表。
	Steps []string `json:"steps"`
	// Queries 是每个步骤对应需要检索的知识点。
	Queries []string `json:"queries"`
}

// ChatService 定义聊天模块对外暴露的业务能力。
type ChatService interface {
	// StreamResponse 执行 RAG 问答，并把 LLM 的流式输出写入 WebSocket。
	StreamResponse(ctx context.Context, query string, user *model.User, ws *websocket.Conn, shouldStop func() bool) error
}

// chatService 是 ChatService 的具体实现。
type chatService struct {
	// searchService 负责根据用户问题检索知识库上下文。
	searchService SearchService
	// llmClient 负责调用大模型流式生成答案。
	llmClient llm.Client
	// conversationRepo 负责从 Redis 读取和保存聊天历史。
	conversationRepo repository.ConversationRepository
	// lightragClient 是图谱检索客户端，nil 时不启用。
	lightragClient *lightrag.Client
}

// NewChatService 创建聊天业务对象，并注入它依赖的搜索服务、LLM 客户端和会话仓储。
// 若配置中 lightrag.enable=true，会自动初始化 LightRAG 客户端。
func NewChatService(searchService SearchService, llmClient llm.Client, conversationRepo repository.ConversationRepository) ChatService {
	svc := &chatService{
		searchService:    searchService,
		llmClient:        llmClient,
		conversationRepo: conversationRepo,
	}
	if config.Conf.LightRAG.Enable {
		svc.lightragClient = lightrag.NewClient(config.Conf.LightRAG)
		log.Infof("[ChatService] LightRAG 已启用, URL: %s", config.Conf.LightRAG.URL)
	}
	return svc
}

// StreamResponse 协调完整 RAG 流程：意图分析、并行检索、组装消息、调用 LLM、流式返回、保存历史。
func (s *chatService) StreamResponse(ctx context.Context, query string, user *model.User, ws *websocket.Conn, shouldStop func() bool) error {
	// 第一步：意图分析——用轻量模型把用户问题拆解成思维步骤和多个子检索词。
	intent, err := s.analyzeIntent(ctx, query)
	if err != nil {
		// 意图分析失败时降级为原始问题直接检索，不中断整个流程。
		log.Warnf("[ChatService] 意图分析失败，降级为单次检索: %v", err)
		intent = &intentResult{Queries: []string{query}}
	}

	// 把意图分析结果（思维链）推送给前端，让用户看到"正在思考"的过程。
	sendThinkingEvent(ws, intent)

	// 第二步：并行检索——对每个子查询同时发起知识库召回，汇总去重后作为上下文。
	results := s.parallelSearch(ctx, intent.Queries, 5, user)

	// 把搜索结果转换成一段可放进 prompt 的参考资料文本。
	contextText := s.buildContextText(results)

	// 第三步（可选）：若 LightRAG 已启用，补充图谱检索上下文。
	// LightRAG 的 mix 模式同时用向量检索和知识图谱推理，可以找到 ES 遗漏的实体关系。
	if s.lightragClient != nil {
		graphCtx, err := s.lightragClient.QueryContext(ctx, query, "mix")
		if err != nil {
			log.Warnf("[ChatService] LightRAG 检索失败，忽略图谱上下文: %v", err)
		} else if strings.TrimSpace(graphCtx) != "" {
			contextText = contextText + "\n\n--- 知识图谱补充信息 ---\n" + graphCtx
			log.Infof("[ChatService] LightRAG 返回图谱上下文，长度: %d 字符", len(graphCtx))
		}
	}

	// 根据参考资料文本构造 system message，告诉模型回答规则和参考内容。
	systemMsg := s.buildSystemMessage(contextText)

	// 从 Redis 读取该用户之前的聊天历史。
	history, err := s.loadHistory(ctx, user.ID)
	// 历史读取失败不阻断本次问答，只记录日志，然后按无历史继续。
	if err != nil {
		log.Errorf("Failed to load conversation history: %v", err)
		history = []model.ChatMessage{}
	}

	// 把 system message、历史消息、当前用户问题组合成 LLM 的 messages。
	messages := s.composeMessages(systemMsg, history, query)

	// answerBuilder 用来收集 LLM 流式输出的完整答案。
	answerBuilder := &strings.Builder{}
	// interceptor 是 WebSocket 写入拦截器：一边转发 chunk，一边把答案写入 answerBuilder。
	interceptor := &wsWriterInterceptor{
		// conn 是真实的 WebSocket 连接。
		conn: ws,
		// writer 用来累计完整答案。
		writer: answerBuilder,
		// shouldStop 用来判断用户是否点击了停止生成。
		shouldStop: shouldStop,
	}

	// 读取模型生成参数，比如 temperature、top_p、max_tokens。
	gen := s.buildGenerationParams()

	// llmMsgs 是传给 LLM 客户端的消息列表。
	llmMsgs := make([]llm.Message, 0, len(messages))
	// 把内部的 model.ChatMessage 转成 pkg/llm 里的 Message。
	for _, m := range messages {
		llmMsgs = append(llmMsgs, llm.Message{
			// Role 表示 system/user/assistant。
			Role: m.Role,
			// Content 是消息正文。
			Content: m.Content,
		})
	}

	// 调用大模型流式生成，LLM 客户端每拿到一个 chunk 就会调用 interceptor.WriteMessage。
	err = s.llmClient.StreamChatMessages(ctx, llmMsgs, gen, interceptor)
	// 如果大模型调用失败，直接返回错误给上层 handler。
	if err != nil {
		return err
	}

	// 模型流式输出结束后，通知前端本轮回答已经完成。
	sendCompletion(ws)

	// 从 answerBuilder 取出完整答案，用来保存聊天历史。
	fullAnswer := answerBuilder.String()
	// 如果确实生成了内容，才保存历史。
	if len(fullAnswer) > 0 {
		// 使用后台 context 保存历史，避免原始请求取消后导致保存失败。
		err = s.addMessageToConversation(context.Background(), user.ID, query, fullAnswer)
		// 保存历史失败不影响本次回答，因为答案已经成功流式返回给前端。
		if err != nil {
			log.Errorf("Failed to save conversation history: %v", err)
		}
	}

	// 整个 RAG 流程正常结束。
	return nil
}

// analyzeIntent 调用轻量 LLM 对用户问题做意图分析，拆解出思维步骤和子检索词列表。
func (s *chatService) analyzeIntent(ctx context.Context, query string) (*intentResult, error) {
	// 构造意图分析专用的 system prompt，要求模型以严格 JSON 格式返回。
	systemPrompt := `你是一个查询分析助手。给定用户问题，请分析：
1. 用户意图（一句话概括）
2. 解决这个问题的思维步骤（2-4个步骤）
3. 每个步骤需要检索的知识点（简洁的检索词）

必须严格以 JSON 格式返回，不要有任何额外文字、代码块标记或注释：
{"intent":"用户意图","steps":["步骤1","步骤2"],"queries":["检索词1","检索词2"]}`

	// 构造发给意图分析模型的消息列表。
	messages := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: query},
	}

	// 确定意图分析使用的模型：优先用配置里的 intent_model，没有就退回主模型。
	intentModel := config.Conf.LLM.IntentModel
	if intentModel == "" {
		intentModel = config.Conf.LLM.Model
	}

	// 构造专用于意图分析的轻量 LLM 配置：低 temperature 保证 JSON 格式稳定。
	intentCfg := config.LLMConfig{
		APIKey:  config.Conf.LLM.APIKey,
		BaseURL: config.Conf.LLM.BaseURL,
		Model:   intentModel,
	}
	intentClient := llm.NewClient(intentCfg)

	// 调用非流式接口，一次性拿到完整 JSON 响应。
	temp := 0.1
	maxTok := 512
	raw, err := intentClient.CompleteChatMessages(ctx, messages, &llm.GenerationParams{
		Temperature: &temp,
		MaxTokens:   &maxTok,
	})
	if err != nil {
		return nil, fmt.Errorf("intent analysis LLM call failed: %w", err)
	}

	log.Infof("[ChatService] 意图分析原始响应: %s", raw)

	// 有时模型会在 JSON 前后附加 markdown 代码块标记，尝试提取其中的 JSON 部分。
	raw = strings.TrimSpace(raw)
	if idx := strings.Index(raw, "{"); idx > 0 {
		raw = raw[idx:]
	}
	if idx := strings.LastIndex(raw, "}"); idx >= 0 && idx < len(raw)-1 {
		raw = raw[:idx+1]
	}

	// 把 JSON 字符串反序列化成 intentResult。
	var result intentResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("failed to parse intent JSON: %w", err)
	}

	// 如果模型没有返回任何 queries，就用原始问题兜底。
	if len(result.Queries) == 0 {
		result.Queries = []string{query}
	}

	log.Infof("[ChatService] 意图分析结果: intent=%s, steps=%d, queries=%v", result.Intent, len(result.Steps), result.Queries)
	return &result, nil
}

// parallelSearch 并发对每个子查询执行知识库召回，合并去重后返回结果。
func (s *chatService) parallelSearch(ctx context.Context, queries []string, topKEach int, user *model.User) []model.SearchResponseDTO {
	// resultCh 收集所有 goroutine 的检索结果。
	resultCh := make(chan []model.SearchResponseDTO, len(queries))

	var wg sync.WaitGroup
	for _, q := range queries {
		wg.Add(1)
		go func(q string) {
			defer wg.Done()
			res, err := s.searchService.HybridSearch(ctx, q, topKEach, user)
			if err != nil {
				log.Warnf("[ChatService] 子查询检索失败 query='%s': %v", q, err)
				return
			}
			resultCh <- res
		}(q)
	}

	// 等所有 goroutine 结束后关闭 channel，避免 range 死锁。
	wg.Wait()
	close(resultCh)

	// 收集结果并按 TextContent 前缀去重，避免多个子查询命中同一片段。
	var merged []model.SearchResponseDTO
	seen := make(map[string]bool)
	for res := range resultCh {
		for _, r := range res {
			// 用文本内容的前 80 字符作为去重 key，平衡精度和性能。
			key := r.TextContent
			if len(key) > 80 {
				key = key[:80]
			}
			if !seen[key] {
				seen[key] = true
				merged = append(merged, r)
			}
		}
	}

	log.Infof("[ChatService] 并行检索完成: queries=%d, merged=%d 条", len(queries), len(merged))
	return merged
}

// sendThinkingEvent 把意图分析结果作为 thinking 事件推送给前端。
func sendThinkingEvent(ws *websocket.Conn, intent *intentResult) {
	payload := map[string]interface{}{
		"type":    "thinking",
		"intent":  intent.Intent,
		"steps":   intent.Steps,
		"queries": intent.Queries,
	}
	b, _ := json.Marshal(payload)
	// 推送失败不影响后续流程，忽略错误。
	_ = ws.WriteMessage(websocket.TextMessage, b)
}

// buildContextText 把搜索结果拼成 prompt 中的参考资料区。
func (s *chatService) buildContextText(searchResults []model.SearchResponseDTO) string {
	// 如果没有搜索结果，就返回空字符串，后面 system prompt 会写入“无检索结果”提示。
	if len(searchResults) == 0 {
		return ""
	}

	// 单条片段最多保留 1000 字符，和 Processor 的切块大小对齐。
	const maxSnippetLen = 1000
	// 使用 strings.Builder 拼接字符串，避免循环中频繁创建新字符串。
	var contextBuilder strings.Builder

	// 遍历每一条检索结果。
	for i, r := range searchResults {
		// snippet 是命中的文本片段。
		snippet := r.TextContent
		// 如果片段太长，就截断，避免 prompt 过长。
		if len(snippet) > maxSnippetLen {
			snippet = snippet[:maxSnippetLen] + "..."
		}

		// fileLabel 是来源文件名。
		fileLabel := r.FileName
		// 如果文件名为空，就用 unknown 兜底。
		if fileLabel == "" {
			fileLabel = "unknown"
		}

		// 每条参考资料格式：[编号] (文件名) 文本内容。
		contextBuilder.WriteString(fmt.Sprintf("[%d] (%s) %s\n", i+1, fileLabel, snippet))
	}

	// 返回拼好的上下文文本。
	return contextBuilder.String()
}

// buildSystemMessage 根据检索上下文构造 system message。
func (s *chatService) buildSystemMessage(contextText string) string {
	// 优先读取原项目兼容 Java 风格的 ai.prompt.rules。
	rules := config.Conf.AI.Prompt.Rules
	// 如果 ai.prompt.rules 没配置，就回退到 llm.prompt.rules。
	if rules == "" {
		rules = config.Conf.LLM.Prompt.Rules
	}

	// 优先读取 ai.prompt.ref-start。
	refStart := config.Conf.AI.Prompt.RefStart
	// 如果 ai 没配置，就回退到 llm.prompt.ref_start。
	if refStart == "" {
		refStart = config.Conf.LLM.Prompt.RefStart
	}
	// 如果两个配置都没有，就使用默认参考资料开始标记。
	if refStart == "" {
		refStart = "<<REF>>"
	}

	// 优先读取 ai.prompt.ref-end。
	refEnd := config.Conf.AI.Prompt.RefEnd
	// 如果 ai 没配置，就回退到 llm.prompt.ref_end。
	if refEnd == "" {
		refEnd = config.Conf.LLM.Prompt.RefEnd
	}
	// 如果两个配置都没有，就使用默认参考资料结束标记。
	if refEnd == "" {
		refEnd = "<<END>>"
	}

	// sys 用来拼接完整 system prompt。
	var sys strings.Builder
	// 如果配置了回答规则，就先写入规则。
	if rules != "" {
		sys.WriteString(rules)
		sys.WriteString("\n\n")
	}

	// 写入参考资料开始标记。
	sys.WriteString(refStart)
	sys.WriteString("\n")

	// 如果有检索上下文，就把检索结果写入参考资料区。
	if contextText != "" {
		sys.WriteString(contextText)
	} else {
		// 如果没有检索结果，优先读取 ai.prompt.no-result-text。
		noRes := config.Conf.AI.Prompt.NoResultText
		// 如果 ai 没配置，就回退到 llm.prompt.no_result_text。
		if noRes == "" {
			noRes = config.Conf.LLM.Prompt.NoResultText
		}
		// 如果仍然没有配置，就使用默认提示。
		if noRes == "" {
			noRes = "（本轮无检索结果）"
		}
		// 把无检索结果提示写入参考资料区。
		sys.WriteString(noRes)
		sys.WriteString("\n")
	}

	// 写入参考资料结束标记。
	sys.WriteString(refEnd)
	// 返回最终 system message。
	return sys.String()
}

// loadHistory 从 Redis 中加载用户聊天历史。
func (s *chatService) loadHistory(ctx context.Context, userID uint) ([]model.ChatMessage, error) {
	// 先根据 userID 获取或创建 conversationID。
	convID, err := s.conversationRepo.GetOrCreateConversationID(ctx, userID)
	// 如果 Redis 操作失败，直接返回错误。
	if err != nil {
		return nil, err
	}

	// 根据 conversationID 查询历史消息列表。
	return s.conversationRepo.GetConversationHistory(ctx, convID)
}

// composeMessages 组装发送给 LLM 的消息列表。
func (s *chatService) composeMessages(systemMsg string, history []model.ChatMessage, userInput string) []model.ChatMessage {
	// 预分配容量：system 一条 + 历史消息 + 当前用户问题。
	msgs := make([]model.ChatMessage, 0, len(history)+2)
	// 第一条必须是 system message，用来约束模型行为。
	msgs = append(msgs, model.ChatMessage{Role: "system", Content: systemMsg})
	// 中间加入历史对话，让模型具备上下文记忆。
	msgs = append(msgs, history...)
	// 最后一条加入当前用户问题。
	msgs = append(msgs, model.ChatMessage{Role: "user", Content: userInput})
	// 返回完整 messages。
	return msgs
}

// addMessageToConversation 把一轮用户问题和模型答案保存到 Redis 历史记录。
func (s *chatService) addMessageToConversation(ctx context.Context, userID uint, question, answer string) error {
	// 获取或创建该用户对应的 conversationID。
	conversationID, err := s.conversationRepo.GetOrCreateConversationID(ctx, userID)
	// 获取失败时返回带上下文的错误。
	if err != nil {
		return fmt.Errorf("failed to get or create conversation ID: %w", err)
	}

	// 读取当前已有历史。
	history, err := s.conversationRepo.GetConversationHistory(ctx, conversationID)
	// 读取失败时返回带上下文的错误。
	if err != nil {
		return fmt.Errorf("failed to get conversation history: %w", err)
	}

	// 追加用户消息。
	history = append(history, model.ChatMessage{
		// Role=user 表示这条消息来自用户。
		Role: "user",
		// Content 保存用户问题。
		Content: question,
		// Timestamp 记录当前时间。
		Timestamp: time.Now(),
	})

	// 追加助手消息。
	history = append(history, model.ChatMessage{
		// Role=assistant 表示这条消息来自模型。
		Role: "assistant",
		// Content 保存完整回答。
		Content: answer,
		// Timestamp 记录当前时间。
		Timestamp: time.Now(),
	})

	// 把追加后的完整历史写回 Redis。
	return s.conversationRepo.UpdateConversationHistory(ctx, conversationID, history)
}

// wsWriterInterceptor 封装 WebSocket 写入逻辑，并同时收集完整回答。
type wsWriterInterceptor struct {
	// conn 是真实的 WebSocket 连接。
	conn *websocket.Conn
	// writer 用来累计模型返回的所有 chunk。
	writer *strings.Builder
	// shouldStop 用来判断前端是否请求停止生成。
	shouldStop func() bool
}

// WriteMessage 实现 llm.MessageWriter 接口。
func (w *wsWriterInterceptor) WriteMessage(messageType int, data []byte) error {
	// 如果前端已经请求停止生成，就跳过下发当前 chunk。
	if w.shouldStop != nil && w.shouldStop() {
		return nil
	}

	// 把当前 chunk 追加到完整回答里。
	w.writer.Write(data)

	// 前端希望收到 JSON，所以把原始文本 chunk 包装成 {"chunk":"..."}。
	payload := map[string]string{
		// chunk 字段保存本次模型增量输出。
		"chunk": string(data),
	}

	// 把 payload 序列化成 JSON。
	b, _ := json.Marshal(payload)

	// 通过 WebSocket 把 JSON chunk 写给前端。
	return w.conn.WriteMessage(messageType, b)
}

// sendCompletion 给前端发送“回答完成”的通知。
func sendCompletion(ws *websocket.Conn) {
	// notif 是完成事件的 JSON 数据。
	notif := map[string]interface{}{
		// type 表示这是完成事件，不是普通文本 chunk。
		"type": "completion",
		// status 表示本轮生成已经结束。
		"status": "finished",
		// message 是前端可展示的提示。
		"message": "响应已完成",
		// timestamp 是毫秒时间戳。
		"timestamp": time.Now().UnixMilli(),
		// date 是可读时间字符串。
		"date": time.Now().Format("2006-01-02T15:04:05"),
	}

	// 把完成事件序列化成 JSON。
	b, _ := json.Marshal(notif)
	// 发送失败也不再返回错误，因为主流程已经结束。
	_ = ws.WriteMessage(websocket.TextMessage, b)
}

// buildGenerationParams 从配置中构造模型生成参数。
func (s *chatService) buildGenerationParams() *llm.GenerationParams {
	// gp 是最终传给 LLM 客户端的生成参数。
	var gp llm.GenerationParams

	// 如果配置了 temperature，就传给模型控制随机性。
	if config.Conf.LLM.Generation.Temperature != 0 {
		t := config.Conf.LLM.Generation.Temperature
		gp.Temperature = &t
	}

	// 如果配置了 top_p，就传给模型控制 nucleus sampling。
	if config.Conf.LLM.Generation.TopP != 0 {
		p := config.Conf.LLM.Generation.TopP
		gp.TopP = &p
	}

	// 如果配置了 max_tokens，就限制模型最大输出长度。
	if config.Conf.LLM.Generation.MaxTokens != 0 {
		m := config.Conf.LLM.Generation.MaxTokens
		gp.MaxTokens = &m
	}

	// 如果三个参数都没配置，就返回 nil，让 LLM 客户端使用默认值。
	if gp.Temperature == nil && gp.TopP == nil && gp.MaxTokens == nil {
		return nil
	}

	// 返回配置好的生成参数。
	return &gp
}
