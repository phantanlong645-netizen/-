package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"RAG-repository/internal/config"
	"RAG-repository/internal/model"
	"RAG-repository/internal/repository"
	"RAG-repository/internal/research"
	"RAG-repository/pkg/llm"
	"RAG-repository/pkg/log"
)

// PaperImporter 将已下载的文件内容导入知识库的接口。
// 由 UploadService 实现，通过接口注入使研究检索服务无需直接依赖 RAG 基础设施。
type PaperImporter interface {
	ImportFile(ctx context.Context, content []byte, fileName, contentType, orgTag string, isPublic bool, userID uint) (*model.FileUpload, error)
}

// ResearchAgentService 学术检索Agent的服务层接口，定义检索、查询和导入候选论文的接口方法
type ResearchAgentService interface {
	// RunSearch 执行学术论文检索，阻塞直到完成再返回结果
	RunSearch(ctx context.Context, user *model.User, req ResearchSearchRequest) (*ResearchSearchResponse, error)
	// RunSearchStream 执行学术论文检索，通过 onEvent 回调实时推送 Agent 进度事件
	RunSearchStream(ctx context.Context, user *model.User, req ResearchSearchRequest, onEvent func(AgentProgressEvent)) (*ResearchSearchResponse, error)
	// ListSessions 查询用户的历史检索会话列表
	ListSessions(user *model.User, limit int) ([]model.ResearchSession, error)
	// ListCandidates 查询指定会话下的所有候选论文列表
	ListCandidates(sessionID uint, user *model.User) ([]model.ResearchCandidate, error)
	// ImportCandidate 将候选论文下载并导入用户的知识库
	ImportCandidate(ctx context.Context, candidateID uint, user *model.User, req ResearchImportRequest) (*model.FileUpload, error)
}

// AgentProgressEvent Agent 运行期间实时推送的进度事件。
// Type 取値："search"、"found"、"select"、"done"、"error"
type AgentProgressEvent struct {
	Type    string `json:"type"`
	Source  string `json:"source,omitempty"`  // search / found
	Query   string `json:"query,omitempty"`   // search / found
	Count   int    `json:"count,omitempty"`   // found
	Title   string `json:"title,omitempty"`   // select
	Reason  string `json:"reason,omitempty"`  // select
	Message string `json:"message,omitempty"` // error / done
}

// ResearchSearchRequest 检索请求结构体
// Query: 用户输入的研究问题
// Limit: 返回的候选论文数量上限
type ResearchSearchRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// ResearchSearchResponse 检索响应结构体
// Session: 本次检索会话信息
// Candidates: 检索到的候选论文列表
type ResearchSearchResponse struct {
	Session    model.ResearchSession     `json:"session"`
	Candidates []model.ResearchCandidate `json:"candidates"`
}

// ResearchImportRequest 导入候选论文的请求结构体
// OrgTag: 组织标签，用于权限控制
// IsPublic: 是否公开
type ResearchImportRequest struct {
	OrgTag   string `json:"orgTag"`
	IsPublic bool   `json:"isPublic"`
}

// researchAgentService 学术检索Agent服务的实现结构体
type researchAgentService struct {
	researchRepo repository.ResearchRepository // 检索相关的数据仓库
	orgTagRepo   repository.OrgTagRepository   // 组织标签相关的数据仓库
	importer     PaperImporter                 // 导入知识库的接口，由 UploadService 实现
	agentCfg     config.ResearchAgentConfig    // Agent配置
	llmClient    llm.Client                    // LLM客户端，用于生成查询计划和LLM筛选
	tools        []research.SearchTool         // 搜索工具列表（如Semantic Scholar、Arxiv）
	httpClient   *http.Client                  // HTTP客户端，用于下载PDF
}

// NewResearchAgentService 创建一个新的学术检索Agent服务实例
// 各依赖通过参数注入，实现依赖反转
func NewResearchAgentService(
	researchRepo repository.ResearchRepository,
	orgTagRepo repository.OrgTagRepository,
	agentCfg config.ResearchAgentConfig,
	llmClient llm.Client,
	importer PaperImporter,
) ResearchAgentService {
	return &researchAgentService{
		researchRepo: researchRepo,
		orgTagRepo:   orgTagRepo,
		importer:     importer,
		agentCfg:     agentCfg,
		llmClient:    llmClient,
		// 初始化搜索工具：Semantic Scholar和Arxiv
		tools: []research.SearchTool{
			research.NewSemanticScholarTool(agentCfg.SemanticScholarAPIKey),
			research.NewArxivTool(),
			research.NewCrossrefTool(),
			research.NewOpenAlexTool(),
			research.NewPubMedTool(),
		},
		// HTTP客户端超时设置为60秒
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// RunSearch 启动 Agent 主循环，自主决策搜索策略，收集候选论文并保存到数据库。
func (s *researchAgentService) RunSearch(ctx context.Context, user *model.User, req ResearchSearchRequest) (*ResearchSearchResponse, error) {
	return s.RunSearchStream(ctx, user, req, nil)
}

// ListCandidates 查询指定会话ID下的所有候选论文列表
func (s *researchAgentService) ListCandidates(sessionID uint, user *model.User) ([]model.ResearchCandidate, error) {
	return s.researchRepo.ListCandidates(sessionID, user.ID)
}

// ListSessions 查询用户的历史检索会话列表
func (s *researchAgentService) ListSessions(user *model.User, limit int) ([]model.ResearchSession, error) {
	return s.researchRepo.ListSessions(user.ID, limit)
}

// RunSearchStream 与 RunSearch 相同，但通过 onEvent 回调实时推送 Agent 进度事件。
// onEvent 在调用线程中同步执行，调用方负责非阻塞地消费事件（如写入 buffered channel）。
func (s *researchAgentService) RunSearchStream(ctx context.Context, user *model.User, req ResearchSearchRequest, onEvent func(AgentProgressEvent)) (*ResearchSearchResponse, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, errors.New("检索问题不能为空")
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	if s.agentCfg.MaxCandidates > 0 && limit > s.agentCfg.MaxCandidates {
		limit = s.agentCfg.MaxCandidates
	}

	session := &model.ResearchSession{
		UserID: user.ID,
		Query:  query,
		Status: model.ResearchSessionStatusCompleted,
	}
	if err := s.researchRepo.CreateSession(session); err != nil {
		return nil, err
	}

	dbCandidates, err := s.runAgentLoop(ctx, session, query, limit, onEvent)
	if err != nil {
		if onEvent != nil {
			onEvent(AgentProgressEvent{Type: "error", Message: err.Error()})
		}
		now := time.Now()
		session.Status = model.ResearchSessionStatusFailed
		session.ErrorMessage = err.Error()
		session.CompletedAt = &now
		_ = s.researchRepo.UpdateSession(session)
		return nil, err
	}

	if err := s.researchRepo.CreateCandidates(dbCandidates); err != nil {
		now := time.Now()
		session.Status = model.ResearchSessionStatusFailed
		session.ErrorMessage = err.Error()
		session.CompletedAt = &now
		_ = s.researchRepo.UpdateSession(session)
		return nil, err
	}

	now := time.Now()
	session.CompletedAt = &now
	if err := s.researchRepo.UpdateSession(session); err != nil {
		return nil, err
	}

	list, err := s.researchRepo.ListCandidates(session.ID, user.ID)
	if err != nil {
		return nil, err
	}
	if onEvent != nil {
		onEvent(AgentProgressEvent{Type: "done", Message: fmt.Sprintf("检索完成，共找到 %d 篇候选论文", len(list)), Count: len(list)})
	}
	return &ResearchSearchResponse{Session: *session, Candidates: list}, nil
}

// ImportCandidate 将候选论文下载并导入用户的知识库
// 流程：1.获取候选论文信息 2.验证组织标签权限 3.下载PDF或生成Markdown 4.委托 PaperImporter 完成存储和消息队列操作
func (s *researchAgentService) ImportCandidate(ctx context.Context, candidateID uint, user *model.User, req ResearchImportRequest) (*model.FileUpload, error) {
	// 根据候选论文ID和用户ID获取候选论文详情
	candidate, err := s.researchRepo.GetCandidate(candidateID, user.ID)
	if err != nil {
		return nil, errors.New("候选结果不存在")
	}

	// 获取组织标签，如果请求中未指定则使用用户的默认组织
	orgTag := strings.TrimSpace(req.OrgTag)
	if orgTag == "" {
		orgTag = user.PrimaryOrg
	}
	if orgTag == "" {
		return nil, errors.New("组织标签不能为空")
	}
	// 验证组织标签是否存在
	if _, err := s.orgTagRepo.FindByID(orgTag); err != nil {
		return nil, errors.New("组织标签不存在")
	}

	// 下载候选论文内容（优先PDF，否则生成Markdown摘要）
	content, fileName, contentType, err := s.loadCandidateContent(ctx, candidate)
	if err != nil {
		s.markCandidateImportFailed(candidate, err)
		return nil, err
	}

	// 委托 PaperImporter 完成文件存储、上传记录和消息队列操作
	record, err := s.importer.ImportFile(ctx, content, fileName, contentType, orgTag, req.IsPublic, user.ID)
	if err != nil {
		s.markCandidateImportFailed(candidate, err)
		return nil, err
	}

	// 更新候选论文的导入状态为已导入
	now := time.Now()
	candidate.ImportStatus = model.ResearchCandidateImportImported
	candidate.FileMD5 = record.FileMD5
	candidate.ImportError = ""
	candidate.ImportedAt = &now
	_ = s.researchRepo.UpdateCandidate(candidate)
	return record, nil
}

// markCandidateImportFailed 标记候选论文导入失败
// 更新候选论文的导入状态和错误信息
func (s *researchAgentService) markCandidateImportFailed(candidate *model.ResearchCandidate, err error) {
	candidate.ImportStatus = model.ResearchCandidateImportFailed
	candidate.ImportError = err.Error()
	_ = s.researchRepo.UpdateCandidate(candidate)
}

// ─── Agent Loop ──────────────────────────────────────────────────────────────

const defaultMaxAgentIterations = 20

// maxToolResultChars 限制单条工具返回结果写入 message 历史的最大字符数。
// 超长内容不影响工具逻辑（state 内全量保存），但将保掤 LLM 上下文窗口不溢出。
const maxToolResultChars = 3000

// pruneThreshold 是触发消息历史裁剪的总字符数阈值（约 15,000 tokens）。
// pruneKeepRounds 是裁剪后保留的最近完整轮数（每轮 = 1 条 assistant + 若干 tool）。
const (
	pruneThreshold  = 60_000
	pruneKeepRounds = 4
)

// agentState 保存单次检索会话中 Agent 的运行状态。
type agentState struct {
	mu              sync.Mutex                // 保护并发工具调用时的字段读写
	searchedPapers  map[string]research.Paper // key: "provider:external_id"
	searchedPairs   map[string]bool           // key: "source|query" - 防止重复搜索同一 source+query 组合
	selectedIDs     []string                  // 已选论文的有序 ID 列表（顺序决定排名）
	selectedReasons map[string]string         // paperID -> 选择原因
	queries         []string                  // Agent 使用过的检索词
	finished        bool
	tokensUsed      int                      // 累计消耗的 token 数量（仅主循环 LLM 调用）
	onEvent         func(AgentProgressEvent) // 可为 nil，非 nil 时实时推送进度事件
}

// runAgentLoop 执行 Agent 主循环：LLM 自主决定搜索策略，收集候选论文。
// onEvent 可为 nil；非 nil 时每个重要动作都会同步回调一次。
func (s *researchAgentService) runAgentLoop(ctx context.Context, session *model.ResearchSession, query string, limit int, onEvent func(AgentProgressEvent)) ([]*model.ResearchCandidate, error) {
	if s.llmClient == nil {
		return nil, errors.New("未配置 LLM 客户端，无法运行检索 Agent")
	}

	maxIter := s.agentCfg.MaxIterations
	if maxIter <= 0 {
		maxIter = defaultMaxAgentIterations
	}

	state := &agentState{
		searchedPapers:  make(map[string]research.Paper),
		searchedPairs:   make(map[string]bool),
		selectedReasons: make(map[string]string),
		onEvent:         onEvent,
	}

	tools := buildAgentTools()
	messages := []llm.Message{
		{Role: "system", Content: buildAgentSystemPrompt(limit)},
		{Role: "user", Content: query},
	}

	stallIter := 0
	prevSelectedCount := 0

	for i := 0; i < maxIter; i++ {
		resp, err := s.llmClient.CompleteWithTools(ctx, messages, tools)
		if err != nil {
			return nil, fmt.Errorf("Agent LLM 调用失败（第 %d 轮）: %w", i+1, err)
		}
		state.tokensUsed += resp.Usage.TotalTokens
		log.Infof("[ResearchAgent] iteration=%d stop_reason=%s tool_calls=%d tokens_total=%d", i+1, resp.StopReason, len(resp.ToolCalls), state.tokensUsed)

		// 将 assistant 消息（含工具调用信息）追加到上下文
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		if resp.StopReason == "stop" || len(resp.ToolCalls) == 0 || state.finished {
			break
		}

		// 并发执行所有工具调用（LLM 可能同时发起多个 search_papers）。
		// 结果按原来顺序收集待 goroutine 全部完成后再写入消息历史。
		type toolResult struct {
			callID  string
			content string
		}
		results := make([]toolResult, len(resp.ToolCalls))
		var wg sync.WaitGroup
		for toolIdx, call := range resp.ToolCalls {
			wg.Add(1)
			go func(idx int, c llm.ToolCall) {
				defer wg.Done()
				res := s.dispatchAgentTool(ctx, c, state, limit)
				if len([]rune(res)) > maxToolResultChars {
					res = string([]rune(res)[:maxToolResultChars]) + `...{"truncated":true}`
				}
				results[idx] = toolResult{callID: c.ID, content: res}
			}(toolIdx, call)
		}
		wg.Wait()

		for _, r := range results {
			messages = append(messages, llm.Message{
				Role:       "tool",
				ToolCallID: r.callID,
				Content:    r.content,
			})
		}
		if state.finished {
			break
		}

		// 停滞检测：若连续 3 轮没有选中新论文，Harness 主动注入提示打破僵局
		state.mu.Lock()
		currentSelectedCount := len(state.selectedIDs)
		state.mu.Unlock()
		if currentSelectedCount > prevSelectedCount {
			stallIter = 0
		} else {
			stallIter++
		}
		prevSelectedCount = currentSelectedCount
		if stallIter >= 3 {
			hint := fmt.Sprintf("⚠️ 你已连续 %d 轮未选中新论文（已选 %d/%d）。请重新思考：换用不同检索词、切换数据源，或适当放宽相关性要求。", stallIter, currentSelectedCount, limit)
			messages = append(messages, llm.Message{Role: "user", Content: hint})
			if state.onEvent != nil {
				state.onEvent(AgentProgressEvent{Type: "think", Message: hint})
			}
			stallIter = 0
		}

		// 消息历史裁剪：超过阈值时丢弃最老的几轮，防止上下文窗口溢出
		messages = pruneMessageHistory(messages, pruneKeepRounds)
	}

	// Reflection Critic Subagent：独立 LLM 对已选论文进行相关性审评，过滤掉偏题内容
	if len(state.selectedIDs) > 0 {
		state.selectedIDs = s.runReflectionAgent(ctx, query, state, onEvent)
	}

	// 将 Agent 使用的检索词写回 session，供前端展示
	if len(state.queries) > 0 {
		plannedJSON, _ := json.Marshal(state.queries)
		session.PlannedQueries = string(plannedJSON)
	}

	// 将选中的论文转换为数据库候选记录，排名越靠前分数越高
	dbCandidates := make([]*model.ResearchCandidate, 0, len(state.selectedIDs))
	for rank, paperID := range state.selectedIDs {
		paper, ok := state.searchedPapers[paperID]
		if !ok {
			continue
		}
		authorsJSON, _ := json.Marshal(paper.Authors)
		dbCandidates = append(dbCandidates, &model.ResearchCandidate{
			SessionID:       session.ID,
			UserID:          session.UserID,
			Provider:        paper.Provider,
			ExternalID:      paper.ExternalID,
			Title:           paper.Title,
			Abstract:        paper.Abstract,
			Authors:         string(authorsJSON),
			Year:            paper.Year,
			URL:             paper.URL,
			PDFURL:          paper.PDFURL,
			CitationCount:   paper.CitationCount,
			RelevanceScore:  float64(len(state.selectedIDs) - rank),
			SelectionReason: state.selectedReasons[paperID],
			ImportStatus:    model.ResearchCandidateImportPending,
		})
	}
	return dbCandidates, nil
}

// dispatchAgentTool 根据工具名称派发工具调用，返回 JSON 字符串结果。
func (s *researchAgentService) dispatchAgentTool(ctx context.Context, call llm.ToolCall, state *agentState, limit int) string {
	switch call.Function.Name {
	case "think":
		var args struct {
			Thought string `json:"thought"`
		}
		_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
		log.Infof("[ResearchAgent] think: %s", truncateString(args.Thought, 200))
		if state.onEvent != nil {
			state.onEvent(AgentProgressEvent{Type: "think", Message: args.Thought})
		}
		return `{"ok":true}`
	case "get_status":
		return toolGetStatus(state, limit)
	case "search_papers":
		return s.toolSearchPapers(ctx, call.Function.Arguments, state)
	case "select_paper":
		return toolSelectPaper(call.Function.Arguments, state, limit)
	case "finish":
		state.mu.Lock()
		state.finished = true
		state.mu.Unlock()
		if state.onEvent != nil {
			var args struct {
				Summary string `json:"summary"`
			}
			_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
			state.onEvent(AgentProgressEvent{Type: "finish", Message: args.Summary})
		}
		return `{"status":"ok","message":"检索任务已完成"}`
	default:
		return fmt.Sprintf(`{"error":"unknown tool %q"}`, call.Function.Name)
	}
}

// toolGetStatus 返回 Agent 当前运行状态的 JSON 字符串。
// LLM 可以用它了解已用查询、已选论文数、距目标还差多少以及 token 耗用等信息。
func toolGetStatus(state *agentState, limit int) string {
	state.mu.Lock()
	defer state.mu.Unlock()
	pdfCount := 0
	for _, p := range state.searchedPapers {
		if strings.TrimSpace(p.PDFURL) != "" {
			pdfCount++
		}
	}
	type selectedSummary struct {
		PaperID string `json:"paper_id"`
		Title   string `json:"title"`
		Reason  string `json:"reason"`
	}
	selected := make([]selectedSummary, 0, len(state.selectedIDs))
	for _, id := range state.selectedIDs {
		p := state.searchedPapers[id]
		selected = append(selected, selectedSummary{
			PaperID: id,
			Title:   p.Title,
			Reason:  state.selectedReasons[id],
		})
	}
	status := map[string]interface{}{
		"queries_used":         state.queries,
		"total_found":          len(state.searchedPapers),
		"total_found_with_pdf": pdfCount,
		"selected_count":       len(state.selectedIDs),
		"target_count":         limit,
		"remaining":            limit - len(state.selectedIDs),
		"tokens_used":          state.tokensUsed,
		"selected":             selected,
	}
	result, _ := json.Marshal(status)
	return string(result)
}

// toolSearchPapers 处理 search_papers 工具调用：搜索指定数据源并将结果存入 agentState。
func (s *researchAgentService) toolSearchPapers(ctx context.Context, arguments string, state *agentState) string {
	var args struct {
		Query  string `json:"query"`
		Source string `json:"source"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return `{"error":"invalid arguments"}`
	}
	if args.Limit <= 0 {
		args.Limit = 10
	}
	if args.Limit > 20 {
		args.Limit = 20
	}

	// 工具参数校验：source 必须是已知数据源之一，防止 Agent 幻觉出不存在的 source
	validSources := []string{"semantic_scholar", "arxiv", "crossref", "openalex", "pubmed"}
	if !containsString(validSources, args.Source) {
		return fmt.Sprintf(`{"error":"invalid source %q, valid sources: %s"}`, args.Source, strings.Join(validSources, ", "))
	}

	// 检测重复 (source, query) 组合，避免浪费 API 调用（加锁保护并发工具调用场景）
	pairKey := args.Source + "|" + args.Query
	state.mu.Lock()
	if state.searchedPairs[pairKey] {
		state.mu.Unlock()
		return fmt.Sprintf(`{"error":"already searched source %q with query %q, try a different query or source"}`, args.Source, args.Query)
	}
	state.searchedPairs[pairKey] = true
	if !containsString(state.queries, args.Query) {
		state.queries = append(state.queries, args.Query)
	}
	state.mu.Unlock()

	var tool research.SearchTool
	for _, t := range s.tools {
		if t.Name() == args.Source {
			tool = t
			break
		}
	}
	if tool == nil {
		return fmt.Sprintf(`{"error":"unknown source %q"}`, args.Source)
	}

	if state.onEvent != nil {
		state.onEvent(AgentProgressEvent{Type: "search", Source: args.Source, Query: args.Query})
	}

	papers, err := tool.Search(ctx, args.Query, args.Limit)
	if err != nil {
		log.Warnf("[ResearchAgent] search failed source=%s query=%s error=%v", args.Source, args.Query, err)
		if state.onEvent != nil {
			state.onEvent(AgentProgressEvent{Type: "error", Source: args.Source, Query: args.Query, Message: err.Error()})
		}
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	log.Infof("[ResearchAgent] search done source=%s query=%s count=%d", args.Source, args.Query, len(papers))
	if state.onEvent != nil {
		state.onEvent(AgentProgressEvent{Type: "found", Source: args.Source, Query: args.Query, Count: len(papers)})
	}

	type paperSummary struct {
		PaperID         string   `json:"paper_id"`
		Title           string   `json:"title"`
		Authors         []string `json:"authors"`
		Year            int      `json:"year"`
		CitationCount   int      `json:"citation_count"`
		RelevanceScore  float64  `json:"relevance_score"`
		Abstract        string   `json:"abstract"`
		HasPDF          bool     `json:"has_pdf"`
		AlreadySelected bool     `json:"already_selected,omitempty"`
	}
	// 在锁保护下将搜索结果存入 state，并读取已选状态（并发工具调用安全）
	state.mu.Lock()
	summaries := make([]paperSummary, 0, len(papers))
	for _, p := range papers {
		key := p.Provider + ":" + p.ExternalID
		state.searchedPapers[key] = p
		_, isSelected := state.selectedReasons[key]
		summaries = append(summaries, paperSummary{
			PaperID:         key,
			Title:           p.Title,
			Authors:         p.Authors,
			Year:            p.Year,
			CitationCount:   p.CitationCount,
			RelevanceScore:  paperRelevanceScore(args.Query, p),
			Abstract:        truncateString(p.Abstract, 300),
			HasPDF:          strings.TrimSpace(p.PDFURL) != "",
			AlreadySelected: isSelected,
		})
	}
	state.mu.Unlock()
	// 按相关性降序排序，确保 LLM 优先看到最贴合 query 的论文
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].RelevanceScore != summaries[j].RelevanceScore {
			return summaries[i].RelevanceScore > summaries[j].RelevanceScore
		}
		if summaries[i].CitationCount != summaries[j].CitationCount {
			return summaries[i].CitationCount > summaries[j].CitationCount
		}
		if summaries[i].Year != summaries[j].Year {
			return summaries[i].Year > summaries[j].Year
		}
		return summaries[i].HasPDF && !summaries[j].HasPDF
	})
	result, _ := json.Marshal(map[string]interface{}{
		"count":  len(summaries),
		"papers": summaries,
	})
	return string(result)
}

// toolSelectPaper 处理 select_paper 工具调用：将指定论文加入候选列表。
func toolSelectPaper(arguments string, state *agentState, limit int) string {
	var args struct {
		PaperID string `json:"paper_id"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return `{"error":"invalid arguments"}`
	}
	state.mu.Lock()
	paper, ok := state.searchedPapers[args.PaperID]
	if !ok {
		state.mu.Unlock()
		return `{"error":"paper not found, call search_papers first"}`
	}
	if _, exists := state.selectedReasons[args.PaperID]; exists {
		count := len(state.selectedIDs)
		state.mu.Unlock()
		return fmt.Sprintf(`{"status":"already_selected","selected_count":%d,"target":%d}`, count, limit)
	}
	state.selectedIDs = append(state.selectedIDs, args.PaperID)
	state.selectedReasons[args.PaperID] = args.Reason
	count := len(state.selectedIDs)
	state.mu.Unlock()
	if state.onEvent != nil {
		state.onEvent(AgentProgressEvent{Type: "select", Title: paper.Title, Reason: args.Reason})
	}
	return fmt.Sprintf(`{"status":"ok","selected_count":%d,"target":%d}`, count, limit)
}

// buildAgentTools 返回 Agent 可用的工具定义列表。
func buildAgentTools() []llm.Tool {
	return []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "think",
				Description: "在搜索或选择之前，先写出你的推理思路和计划。不消耗外部资源。",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"thought": {
							"type": "string",
							"description": "你的推理过程、下一步计划或尚未解决的问题"
						}
					},
					"required": ["thought"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "get_status",
				Description: "查询当前检索状态：已用检索词、已找到论文数、已选论文数、距目标还差多少。在制定下一步计划前调用。",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {}
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "search_papers",
				Description: "搜索学术数据库，获取论文列表。每次调用只使用一个数据源，可多次调用以覆盖不同数据源或关键词。",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"query": {
							"type": "string",
							"description": "英文搜索关键词"
						},
						"source": {
							"type": "string",
							"enum": ["semantic_scholar", "arxiv", "crossref", "openalex", "pubmed"],
							"description": "学术数据源名称"
						},
						"limit": {
							"type": "integer",
							"description": "返回结果数量，建议 5-15",
							"default": 10
						}
					},
					"required": ["query", "source"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "select_paper",
				Description: "将搜索结果中的一篇论文加入候选列表。必须先调用 search_papers 获取 paper_id 后才能使用。",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"paper_id": {
							"type": "string",
							"description": "论文唯一标识，来自 search_papers 返回的 paper_id 字段"
						},
						"reason": {
							"type": "string",
							"description": "选择该论文的理由，说明与研究问题的相关性"
						}
					},
					"required": ["paper_id", "reason"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "finish",
				Description: "完成检索任务。当候选列表已达到目标数量，或已充分搜索无法找到更多相关论文时调用。",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"summary": {
							"type": "string",
							"description": "本次检索的简要总结"
						}
					},
					"required": ["summary"]
				}`),
			},
		},
	}
}

// buildAgentSystemPrompt 构建 Agent 系统提示词。
func buildAgentSystemPrompt(limit int) string {
	return fmt.Sprintf(`你是一个自主学术论文检索 Agent。根据用户给出的研究问题，自主制定搜索策略，找到最相关的学术论文。

可用工具：
- think：写出推理思路，不消耗外部资源。建议在每个大步骤前使用。
- get_status：查看当前进度（已用查询、已选论文数、距目标还差多少）。
- search_papers：搜索一个数据源。
- select_paper：选中一篇论文加入候选列表。
- finish：完成任务。

工作流程：
1. think：分析研究问题，列出 2‑3 个英文检索方向和目标数据源
2. search_papers：依次搜索不同数据源和关键词
3. select_paper：对每批搜索结果评估相关性，选择最合适的
4. 每 3‑4 次搜索后调用 get_status 确认进度
5. think：如果候选数不足，思考换用什么关键词或数据源
6. 达到目标数量（约 %d 篇）或已充分搜索时，调用 finish

选择标准：
	- 优先选择与研究问题最贴合的论文
	- 优先选择引用次数多的高影响力论文
	- PDF 只是下载和导入的便利条件，不是 select_paper 的硬约束
	- 没有 PDF 的论文也可以选择，只要它最贴合 query
	- 覆盖研究问题的不同角度和子主题`, limit)
}

// runReflectionAgent 是独立的 Critic Subagent：用一次 LLM 调用对主 Agent 选中的论文逐篇
// 进行相关性审核，返回通过审核的 paper_id 有序列表。未通过审核的论文被过滤掉。
// 若 LLM 调用失败或响应无法解析，则原样返回 selectedIDs（fallback 保留全部结果）。
func (s *researchAgentService) runReflectionAgent(ctx context.Context, query string, state *agentState, onEvent func(AgentProgressEvent)) []string {
	if s.llmClient == nil {
		return state.selectedIDs
	}
	total := len(state.selectedIDs)
	if onEvent != nil {
		onEvent(AgentProgressEvent{Type: "reflect", Message: fmt.Sprintf("Critic agent 正在审核 %d 篇候选论文…", total)})
	}

	type paperForCritic struct {
		PaperID  string `json:"paper_id"`
		Title    string `json:"title"`
		Abstract string `json:"abstract"`
		HasPDF   bool   `json:"has_pdf"`
		Reason   string `json:"selection_reason"`
	}
	papers := make([]paperForCritic, 0, total)
	for _, id := range state.selectedIDs {
		p := state.searchedPapers[id]
		papers = append(papers, paperForCritic{
			PaperID:  id,
			Title:    p.Title,
			Abstract: truncateString(p.Abstract, 200),
			HasPDF:   strings.TrimSpace(p.PDFURL) != "",
			Reason:   state.selectedReasons[id],
		})
	}
	papersJSON, _ := json.Marshal(papers)

	systemPrompt := `你是学术论文相关性审核 Agent（Critic）。给定研究问题和候选论文列表，对每篇论文独立判断是否与研究问题真正相关。
严格标准：主题偏离、仅边缘相关或内容过于宽泛的论文应拒绝。
has_pdf 只是参考信息，不是硬约束。只要论文与研究问题真正相关，即使没有 PDF 也可以 keep=true。
只输出 JSON 数组，格式：[{"paper_id":"...","keep":true,"reason":"..."}]，不要任何其他内容。`
	userMsg := fmt.Sprintf("研究问题：%s\n\n候选论文：%s", query, string(papersJSON))

	raw, err := s.llmClient.CompleteChatMessages(ctx, []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMsg},
	}, nil)
	if err != nil {
		log.Warnf("[ResearchAgent] reflection agent failed: %v", err)
		return state.selectedIDs
	}

	type decision struct {
		PaperID string `json:"paper_id"`
		Keep    bool   `json:"keep"`
		Reason  string `json:"reason"`
	}
	var decisions []decision
	if err := json.Unmarshal([]byte(extractJSONArray(raw)), &decisions); err != nil {
		log.Warnf("[ResearchAgent] reflection agent parse failed: %v — raw: %s", err, truncateString(raw, 200))
		return state.selectedIDs
	}

	keepSet := make(map[string]bool, len(decisions))
	for _, d := range decisions {
		keepSet[d.PaperID] = d.Keep
		if onEvent != nil {
			if d.Keep {
				onEvent(AgentProgressEvent{Type: "reflect_keep", Title: state.searchedPapers[d.PaperID].Title, Reason: d.Reason})
			} else {
				onEvent(AgentProgressEvent{Type: "reflect_reject", Title: state.searchedPapers[d.PaperID].Title, Reason: d.Reason})
			}
		}
	}

	kept := make([]string, 0, total)
	rejected := 0
	for _, id := range state.selectedIDs {
		if keepSet[id] {
			kept = append(kept, id)
		} else {
			rejected++
		}
	}
	if onEvent != nil {
		onEvent(AgentProgressEvent{Type: "reflect", Message: fmt.Sprintf("Critic agent 完成：保留 %d 篇，拒绝 %d 篇", len(kept), rejected), Count: len(kept)})
	}
	log.Infof("[ResearchAgent] reflection done: kept=%d rejected=%d", len(kept), rejected)
	return kept
}

// extractJSONArray 从 LLM 响应字符串中提取第一个 JSON 数组（处理 markdown 代码块包裹的情况）。
// pruneMessageHistory 在消息历史字符总量超过 pruneThreshold 时裁剪历史。
// 始终保留：messages[0]（system）、messages[1]（初始 user query），
// 以及最近 keepRounds 个完整轮次（每轮 = 1 条 assistant + 若干 tool 结果）。
// 丢弃的轮次替换为一条简短占位符，让 LLM 知晓上下文已被裁剪。
func pruneMessageHistory(messages []llm.Message, keepRounds int) []llm.Message {
	if len(messages) <= 2 {
		return messages
	}
	// 估算总字符数（粗略 token 代理：1 token ≈ 4 chars）
	total := 0
	for _, m := range messages {
		total += len(m.Content)
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Arguments)
		}
	}
	if total <= pruneThreshold {
		return messages
	}

	// 从 messages[2:] 提取每轮（assistant 开头，紧跟其 tool 结果）
	rest := messages[2:]
	type round struct{ start, end int }
	var rounds []round
	i := 0
	for i < len(rest) {
		if rest[i].Role == "assistant" {
			j := i + 1
			for j < len(rest) && rest[j].Role == "tool" {
				j++
			}
			rounds = append(rounds, round{i, j})
			i = j
		} else {
			// injected user hint（停滞检测注入）随相邻轮一起处理，此处跳过
			i++
		}
	}
	if len(rounds) <= keepRounds {
		return messages
	}

	pruned := len(rounds) - keepRounds
	kept := make([]llm.Message, 0, 2+1+keepRounds*5)
	kept = append(kept, messages[0], messages[1])
	kept = append(kept, llm.Message{
		Role:    "user",
		Content: fmt.Sprintf("[Harness 已裁剪 %d 轮早期消息历史，保留最近 %d 轮，避免上下文溢出。已搜索/已选状态请用 get_status 查询。]", pruned, keepRounds),
	})
	for _, r := range rounds[len(rounds)-keepRounds:] {
		kept = append(kept, rest[r.start:r.end]...)
	}
	log.Infof("[ResearchAgent] message history pruned: dropped %d rounds, kept %d rounds, new len=%d", pruned, keepRounds, len(kept))
	return kept
}

func extractJSONArray(s string) string {
	// 去除 ```json ... ``` 或 ``` ... ``` 包裹
	if idx := strings.Index(s, "```json"); idx >= 0 {
		s = s[idx+7:]
		if end := strings.Index(s, "```"); end >= 0 {
			s = s[:end]
		}
	} else if idx := strings.Index(s, "```"); idx >= 0 {
		s = s[idx+3:]
		if end := strings.Index(s, "```"); end >= 0 {
			s = s[:end]
		}
	}
	// 找到第一个 [ 开始的位置
	if idx := strings.Index(s, "["); idx >= 0 {
		s = s[idx:]
	}
	return strings.TrimSpace(s)
}

// truncateString 将字符串截断到指定长度（按 Unicode 字符计），超长则追加省略号。
func truncateString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

func paperRelevanceScore(query string, paper research.Paper) float64 {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return 0
	}

	title := strings.ToLower(strings.TrimSpace(paper.Title))
	abstract := strings.ToLower(strings.TrimSpace(paper.Abstract))

	score := 0.0
	if title == q {
		score += 80
	}
	if strings.Contains(title, q) {
		score += 60
	}
	if strings.Contains(abstract, q) {
		score += 20
	}

	for _, term := range splitQueryTerms(q) {
		if term == "" {
			continue
		}
		if strings.Contains(title, term) {
			score += 8
		}
		if strings.Contains(abstract, term) {
			score += 2
		}
	}

	return score + math.Log1p(float64(maxInt(paper.CitationCount, 0)))
}

func splitQueryTerms(q string) []string {
	return strings.FieldsFunc(q, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// loadCandidateContent 下载候选论文的内容
// 优先下载PDF文件，如果PDF不可用则生成Markdown格式的摘要
// 返回：文件内容、文件名、内容类型
func (s *researchAgentService) loadCandidateContent(ctx context.Context, candidate *model.ResearchCandidate) ([]byte, string, string, error) {
	pdfURL := strings.TrimSpace(candidate.PDFURL)
	if pdfURL == "" {
		return nil, "", "", errors.New("候选论文没有可下载 PDF，不能导入")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pdfURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("Accept", "application/pdf,*/*;q=0.8")
	req.Header.Set("User-Agent", "RAG-repository/1.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("下载 PDF 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("下载 PDF 失败: HTTP %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 100*1024*1024))
	if err != nil {
		return nil, "", "", err
	}
	if !looksLikePDF(body) {
		return nil, "", "", fmt.Errorf("PDF URL 未返回有效 PDF，Content-Type=%q, size=%d bytes", resp.Header.Get("Content-Type"), len(body))
	}

	return body, sanitizeFileName(candidate.Title, ".pdf"), "application/pdf", nil
}

func looksLikePDF(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	sample := body
	if len(sample) > 1024 {
		sample = sample[:1024]
	}
	sample = bytes.TrimPrefix(sample, []byte{0xEF, 0xBB, 0xBF})
	sample = bytes.TrimLeft(sample, "\x00\t\r\n ")
	return bytes.HasPrefix(sample, []byte("%PDF-"))
}

// buildMarkdownCandidate 将候选论文转换为Markdown格式
// 包含标题、来源、年份、URL、作者和摘要
func buildMarkdownCandidate(candidate *model.ResearchCandidate) string {
	var authors []string
	// 解析作者JSON字符串
	_ = json.Unmarshal([]byte(candidate.Authors), &authors)
	return fmt.Sprintf("# %s\n\n- Provider: %s\n- Year: %d\n- URL: %s\n- PDF: %s\n- Authors: %s\n\n## Abstract\n\n%s\n",
		candidate.Title,
		candidate.Provider,
		candidate.Year,
		candidate.URL,
		candidate.PDFURL,
		strings.Join(authors, ", "),
		candidate.Abstract,
	)
}

// sanitizeFileName 清理文件名中的非法字符
// Windows文件系统不支持：\/:*?"<>|
func sanitizeFileName(title string, ext string) string {
	name := strings.TrimSpace(title)
	if name == "" {
		name = "research-paper"
	}
	// 替换非法字符为下划线
	name = regexp.MustCompile(`[\\/:*?"<>|]+`).ReplaceAllString(name, "_")
	name = strings.TrimSpace(name)
	// 限制文件名最大长度120字符
	if len([]rune(name)) > 120 {
		runes := []rune(name)
		name = string(runes[:120])
	}
	// 如果没有扩展名，添加指定的扩展名
	if filepath.Ext(name) == "" {
		name += ext
	}
	return name
}

// containsString 检查切片中是否包含指定字符串
func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
