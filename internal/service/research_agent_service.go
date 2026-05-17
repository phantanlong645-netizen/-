package service

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
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

	"RAG-repository/internal/config"
	"RAG-repository/internal/model"
	"RAG-repository/internal/repository"
	"RAG-repository/internal/research"
	"RAG-repository/internal/storagepath"
	"RAG-repository/pkg/kafka"
	"RAG-repository/pkg/llm"
	"RAG-repository/pkg/log"
	"RAG-repository/pkg/storage"
	"RAG-repository/pkg/tasks"

	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
)

// ResearchAgentService 学术检索Agent的服务层接口，定义检索、查询和导入候选论文的接口方法
type ResearchAgentService interface {
	// RunSearch 执行学术论文检索，根据用户问题搜索外部论文资源并返回候选结果
	RunSearch(ctx context.Context, user *model.User, req ResearchSearchRequest) (*ResearchSearchResponse, error)
	// ListCandidates 查询指定会话下的所有候选论文列表
	ListCandidates(sessionID uint, user *model.User) ([]model.ResearchCandidate, error)
	// ImportCandidate 将候选论文下载并导入用户的知识库
	ImportCandidate(ctx context.Context, candidateID uint, user *model.User, req ResearchImportRequest) (*model.FileUpload, error)
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
	uploadRepo   repository.UploadRepository   // 文件上传相关的数据仓库
	orgTagRepo   repository.OrgTagRepository   // 组织标签相关的数据仓库
	minioCfg     config.MinIOConfig            // MinIO对象存储配置
	agentCfg     config.ResearchAgentConfig    // Agent配置
	llmClient    llm.Client                    // LLM客户端，用于生成查询计划和LLM筛选
	tools        []research.SearchTool         // 搜索工具列表（如Semantic Scholar、Arxiv）
	httpClient   *http.Client                  // HTTP客户端，用于下载PDF
}

// NewResearchAgentService 创建一个新的学术检索Agent服务实例
// 各依赖通过参数注入，实现依赖反转
func NewResearchAgentService(
	researchRepo repository.ResearchRepository,
	uploadRepo repository.UploadRepository,
	orgTagRepo repository.OrgTagRepository,
	minioCfg config.MinIOConfig,
	agentCfg config.ResearchAgentConfig,
	llmClient llm.Client,
) ResearchAgentService {
	return &researchAgentService{
		researchRepo: researchRepo,
		uploadRepo:   uploadRepo,
		orgTagRepo:   orgTagRepo,
		minioCfg:     minioCfg,
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

// RunSearch 执行完整的学术论文检索流程
// 1. 验证并规范化用户输入
// 2. 使用LLM生成多个检索query
// 3. 并行搜索多个学术数据库
// 4. 综合评分排序候选论文
// 5. 保存检索结果到数据库
func (s *researchAgentService) RunSearch(ctx context.Context, user *model.User, req ResearchSearchRequest) (*ResearchSearchResponse, error) {
	// 去除查询字符串的首尾空白字符
	query := strings.TrimSpace(req.Query)
	// 如果查询为空，返回错误
	if query == "" {
		return nil, errors.New("检索问题不能为空")
	}

	// 设置返回结果数量限制，默认10条
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	// 如果配置了最大候选数量且当前limit超过限制，则使用最大限制
	if s.agentCfg.MaxCandidates > 0 && limit > s.agentCfg.MaxCandidates {
		limit = s.agentCfg.MaxCandidates
	}

	// 调用planQueries方法，使用LLM生成多个检索query
	plannedQueries := s.planQueries(ctx, query)
	// 将生成的query列表序列化为JSON格式，用于存储到会话记录中
	plannedJSON, _ := json.Marshal(plannedQueries)

	// 创建检索会话记录
	session := &model.ResearchSession{
		UserID:         user.ID,
		Query:          query,
		PlannedQueries: string(plannedJSON),
		Status:         model.ResearchSessionStatusCompleted,
	}
	// 保存会话到数据库
	if err := s.researchRepo.CreateSession(session); err != nil {
		return nil, err
	}

	// 使用所有生成的query并行搜索所有学术数据库
	papers := s.searchAll(ctx, plannedQueries, limit)
	// 对搜索结果进行综合评分和筛选
	candidates := s.selectCandidates(ctx, query, plannedQueries, papers, limit)

	// 将候选论文转换为数据库模型并批量插入
	dbCandidates := make([]*model.ResearchCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		// 将作者列表序列化为JSON字符串存储
		authorsJSON, _ := json.Marshal(candidate.Authors)
		dbCandidates = append(dbCandidates, &model.ResearchCandidate{
			SessionID:       session.ID,
			UserID:          user.ID,
			Provider:        candidate.Provider,
			ExternalID:      candidate.ExternalID,
			Title:           candidate.Title,
			Abstract:        candidate.Abstract,
			Authors:         string(authorsJSON),
			Year:            candidate.Year,
			URL:             candidate.URL,
			PDFURL:          candidate.PDFURL,
			CitationCount:   candidate.CitationCount,
			RelevanceScore:  candidate.score,
			SelectionReason: candidate.reason,
			ImportStatus:    model.ResearchCandidateImportPending,
		})
	}

	// 批量创建候选论文记录
	if err := s.researchRepo.CreateCandidates(dbCandidates); err != nil {
		now := time.Now()
		// 如果创建失败，更新会话状态为失败并记录错误信息
		session.Status = model.ResearchSessionStatusFailed
		session.ErrorMessage = err.Error()
		session.CompletedAt = &now
		_ = s.researchRepo.UpdateSession(session)
		return nil, err
	}

	// 更新会话完成时间
	now := time.Now()
	session.CompletedAt = &now
	if err := s.researchRepo.UpdateSession(session); err != nil {
		return nil, err
	}

	// 查询并返回本次会话的所有候选论文
	list, err := s.researchRepo.ListCandidates(session.ID, user.ID)
	if err != nil {
		return nil, err
	}
	return &ResearchSearchResponse{Session: *session, Candidates: list}, nil
}

// ListCandidates 查询指定会话ID下的所有候选论文列表
func (s *researchAgentService) ListCandidates(sessionID uint, user *model.User) ([]model.ResearchCandidate, error) {
	return s.researchRepo.ListCandidates(sessionID, user.ID)
}

// ImportCandidate 将候选论文下载并导入用户的知识库
// 流程：1.获取候选论文信息 2.验证组织标签权限 3.下载PDF或生成Markdown 4.上传到MinIO
// 5.创建上传记录 6.发送Kafka消息触发向量化和文件处理任务
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

	// 计算文件MD5值，用于去重和标识
	sum := md5.Sum(content)
	fileMD5 := hex.EncodeToString(sum[:])
	// 生成MinIO对象存储的路径：用户ID/MD5/文件名
	objectName := storagepath.MergedObjectName(user.ID, fileMD5, fileName)
	// 上传文件到MinIO对象存储
	if _, err := storage.MinioClient.PutObject(ctx, s.minioCfg.BucketName, objectName, bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{ContentType: contentType}); err != nil {
		s.markCandidateImportFailed(candidate, err)
		return nil, err
	}

	// 查询是否已存在相同的文件上传记录（基于MD5和用户ID）
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, user.ID)
	if err != nil {
		// 如果记录不存在（gorm.ErrRecordNotFound），则创建新记录
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			s.cleanupImportedObject(ctx, objectName)
			s.markCandidateImportFailed(candidate, err)
			return nil, err
		}
		// 创建新的文件上传记录
		record = &model.FileUpload{
			FileMD5:                   fileMD5,
			FileName:                  fileName,
			TotalSize:                 int64(len(content)),
			Status:                    model.FileUploadStatusCompleted,
			VectorizationStatus:       model.VectorizationStatusPending,
			VectorizationErrorMessage: "",
			UserID:                    user.ID,
			OrgTag:                    orgTag,
			IsPublic:                  req.IsPublic,
		}
		if err := s.uploadRepo.CreateFileUploadRecord(record); err != nil {
			s.cleanupImportedObject(ctx, objectName)
			s.markCandidateImportFailed(candidate, err)
			return nil, err
		}
	} else {
		// 如果记录已存在，更新相关信息（可能文件名、权限等有变化）
		record.FileName = fileName
		record.TotalSize = int64(len(content))
		record.Status = model.FileUploadStatusCompleted
		record.VectorizationStatus = model.VectorizationStatusPending
		record.VectorizationErrorMessage = ""
		record.OrgTag = orgTag
		record.IsPublic = req.IsPublic
		if err := s.uploadRepo.UpdateFileUploadRecord(record); err != nil {
			s.cleanupImportedObject(ctx, objectName)
			s.markCandidateImportFailed(candidate, err)
			return nil, err
		}
	}

	// 构造文件处理任务消息
	task := tasks.FileProcessingTask{
		FileMD5:  fileMD5,
		FileName: fileName,
		UserID:   user.ID,
		OrgTag:   orgTag,
		IsPublic: req.IsPublic,
	}
	// 重置Kafka中该文件的处理尝试次数
	if err := kafka.ResetFileTaskAttempts(fileMD5); err != nil {
		_ = s.uploadRepo.UpdateFileVectorizationStatus(record.ID, model.VectorizationStatusFailed, "reset file task attempts failed: "+err.Error())
		s.markCandidateImportFailed(candidate, err)
		return nil, err
	}
	// 发送文件处理任务到Kafka队列，触发后续的向量化和文件处理流程
	if err := kafka.ProduceFileTask(task); err != nil {
		_ = s.uploadRepo.UpdateFileVectorizationStatus(record.ID, model.VectorizationStatusFailed, "send file processing task to Kafka failed: "+err.Error())
		s.markCandidateImportFailed(candidate, err)
		return nil, err
	}

	// 更新候选论文的导入状态为已导入
	now := time.Now()
	candidate.ImportStatus = model.ResearchCandidateImportImported
	candidate.FileMD5 = fileMD5
	candidate.ImportError = ""
	candidate.ImportedAt = &now
	_ = s.researchRepo.UpdateCandidate(candidate)
	record.VectorizationStatus = model.VectorizationStatusPending
	return record, nil
}

// cleanupImportedObject 清理已上传到MinIO的对象
// 当导入过程出现错误时，调用此方法删除已上传的文件以避免占用存储空间
func (s *researchAgentService) cleanupImportedObject(_ context.Context, objectName string) {
	_ = storage.MinioClient.RemoveObject(context.Background(), s.minioCfg.BucketName, objectName, minio.RemoveObjectOptions{})
}

// markCandidateImportFailed 标记候选论文导入失败
// 更新候选论文的导入状态和错误信息
func (s *researchAgentService) markCandidateImportFailed(candidate *model.ResearchCandidate, err error) {
	candidate.ImportStatus = model.ResearchCandidateImportFailed
	candidate.ImportError = err.Error()
	_ = s.researchRepo.UpdateCandidate(candidate)
}

// scoredPaper 带评分信息的论文结构体
// Paper: 论文基本信息
// score: 综合评分（0-100）
// reason: 评分/筛选原因
type scoredPaper struct {
	research.Paper
	score  float64
	reason string
}

// planQueries 使用LLM为用户的研究问题生成多个检索query
// 返回一个包含原始query和扩展query的列表，最多5个
func (s *researchAgentService) planQueries(ctx context.Context, query string) []string {
	// 初始化时包含原始查询
	queries := []string{query}
	// 如果没有配置LLM客户端，直接返回原始查询
	if s.llmClient == nil {
		return queries
	}

	// 构建prompt，要求LLM生成3个英文学术检索query
	prompt := fmt.Sprintf("请根据研究问题生成 3 个英文学术论文检索 query，只返回 JSON 数组。问题：%s", query)
	temp := 0.2      // 低温度值，保证输出的确定性
	maxTokens := 300 // 最大token数限制
	// 调用LLM完成chat消息
	text, err := s.llmClient.CompleteChatMessages(ctx, []llm.Message{
		{Role: "system", Content: "你是学术搜索 Agent 的 Query Planner。"},
		{Role: "user", Content: prompt},
	}, &llm.GenerationParams{Temperature: &temp, MaxTokens: &maxTokens})
	if err != nil {
		return queries
	}

	// 解析LLM返回的JSON数组
	var planned []string
	if err := json.Unmarshal(extractJSON(text), &planned); err != nil {
		return queries
	}
	// 将LLM生成的query添加到列表中
	for _, item := range planned {
		item = strings.TrimSpace(item)
		// 去重：只添加非空且不重复的query
		if item != "" && !containsString(queries, item) {
			queries = append(queries, item)
		}
		// 最多返回5个query
		if len(queries) >= 5 {
			break
		}
	}
	return queries
}

// searchAll 使用所有query和所有搜索工具并行搜索学术数据库
// seen用于去重，基于Provider:ExternalID作为唯一键
func (s *researchAgentService) searchAll(ctx context.Context, queries []string, limit int) []research.Paper {
	// seen集合用于去重，记录已处理过的论文key
	seen := make(map[string]struct{})
	var papers []research.Paper
	// 每个query的返回数量限制，最小5篇
	perQueryLimit := limit
	if perQueryLimit < 5 {
		perQueryLimit = 5
	}

	// 使用互斥锁保护共享变量papers
	var mu sync.Mutex
	var wg sync.WaitGroup
	// 遍历所有query和所有搜索工具，并行执行搜索
	for _, query := range queries {
		for _, tool := range s.tools {
			query := query
			tool := tool
			wg.Add(1)
			// 启动goroutine并行搜索
			go func() {
				defer wg.Done()

				// 使用当前tool搜索当前query
				results, err := tool.Search(ctx, query, perQueryLimit)
				if err != nil {
					log.Warnf("[ResearchAgent] search source failed, source: %s, query: %s, error: %v", tool.Name(), query, err)
					return
				}
				log.Infof("[ResearchAgent] search source returned, source: %s, query: %s, count: %d", tool.Name(), query, len(results))
				// 加锁保护共享的papers切片
				mu.Lock()
				defer mu.Unlock()
				for _, paper := range results {
					// 生成唯一key用于去重
					key := paper.Provider + ":" + paper.ExternalID
					if _, ok := seen[key]; ok {
						continue
					}
					seen[key] = struct{}{}
					papers = append(papers, paper)
				}
			}()
		}
	}
	// 等待所有搜索任务完成
	wg.Wait()
	return papers
}

// selectCandidates 对搜索结果进行综合评分和排序
// 1. 基于引用量、关键词匹配、PDF可用性等进行基础评分
// 2. 使用LLM进行二次筛选和评分
// 3. 返回最终排序的候选论文列表
func (s *researchAgentService) selectCandidates(ctx context.Context, query string, plannedQueries []string, papers []research.Paper, limit int) []scoredPaper {
	scored := make([]scoredPaper, 0, len(papers))
	// 对每篇论文进行基础评分
	for _, paper := range papers {
		score := scorePaper(plannedQueries, paper)
		scored = append(scored, scoredPaper{
			Paper:  paper,
			score:  score,
			reason: "按标题、摘要关键词命中和引用量综合排序",
		})
	}

	// 按评分降序排序
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	// 截取前limit条
	if len(scored) > limit {
		shortlistSize := limit * 3
		if shortlistSize < limit {
			shortlistSize = limit
		}
		if shortlistSize > 30 {
			shortlistSize = 30
		}
		if shortlistSize > len(scored) {
			shortlistSize = len(scored)
		}
		scored = scored[:shortlistSize]
	}

	// 使用LLM进行二次筛选和评分
	s.applyLLMSelection(ctx, query, scored)
	// 再次按评分排序（LLM可能调整了评分）
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

// applyLLMSelection 使用LLM对候选论文进行二次筛选
// LLM根据标题和摘要判断论文与研究问题的相关性，并给出0-100的评分
func (s *researchAgentService) applyLLMSelection(ctx context.Context, query string, scored []scoredPaper) {
	// 如果没有LLM客户端或没有候选论文，直接返回
	if s.llmClient == nil || len(scored) == 0 {
		return
	}

	// 定义用于LLM的论文结构体
	type candidateForLLM struct {
		Index    int    `json:"index"`
		Title    string `json:"title"`
		Abstract string `json:"abstract"`
		Year     int    `json:"year"`
	}
	// 构建LLM输入数据
	items := make([]candidateForLLM, 0, len(scored))
	for i, item := range scored {
		items = append(items, candidateForLLM{Index: i, Title: item.Title, Abstract: item.Abstract, Year: item.Year})
	}
	// 序列化为JSON
	raw, _ := json.Marshal(items)
	// 构建prompt，要求LLM返回每个候选的评分和原因
	prompt := fmt.Sprintf("用户研究问题：%s\n候选论文：%s\n请返回 JSON 数组，每项包含 index、score(0-100)、reason。", query, string(raw))
	temp := 0.1 // 极低温度，保证输出稳定性
	maxTokens := 1000
	// 调用LLM
	text, err := s.llmClient.CompleteChatMessages(ctx, []llm.Message{
		{Role: "system", Content: "你是学术论文 Selector，只根据标题和摘要判断相关性。"},
		{Role: "user", Content: prompt},
	}, &llm.GenerationParams{Temperature: &temp, MaxTokens: &maxTokens})
	if err != nil {
		log.Warnf("[ResearchAgent] LLM selection failed, error: %v", err)
		return
	}

	// 解析LLM返回的评分结果
	var results []struct {
		Index  int     `json:"index"`
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	}
	if err := json.Unmarshal(extractJSON(text), &results); err != nil {
		log.Warnf("[ResearchAgent] LLM selection parse failed, error: %v", err)
		return
	}
	// 更新候选论文的评分和原因
	for _, result := range results {
		// 跳过无效索引
		if result.Index < 0 || result.Index >= len(scored) {
			continue
		}
		// 更新评分（仅当评分大于0时）
		if result.Score > 0 {
			scored[result.Index].score = result.Score
		}
		// 更新原因说明
		if strings.TrimSpace(result.Reason) != "" {
			scored[result.Index].reason = strings.TrimSpace(result.Reason)
		}
	}
}

// loadCandidateContent 下载候选论文的内容
// 优先下载PDF文件，如果PDF不可用则生成Markdown格式的摘要
// 返回：文件内容、文件名、内容类型
func (s *researchAgentService) loadCandidateContent(ctx context.Context, candidate *model.ResearchCandidate) ([]byte, string, string, error) {
	// 如果有PDF URL，尝试下载PDF
	if strings.TrimSpace(candidate.PDFURL) != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, candidate.PDFURL, nil)
		if err == nil {
			resp, err := s.httpClient.Do(req)
			if err == nil {
				defer resp.Body.Close()
				// 下载成功，读取内容（限制100MB）
				if resp.StatusCode == http.StatusOK {
					body, err := io.ReadAll(io.LimitReader(resp.Body, 100*1024*1024))
					if err != nil {
						return nil, "", "", err
					}
					return body, sanitizeFileName(candidate.Title, ".pdf"), "application/pdf", nil
				}
			}
		}
	}

	// 如果无法下载PDF，生成Markdown格式的摘要
	content := buildMarkdownCandidate(candidate)
	return []byte(content), sanitizeFileName(candidate.Title, ".md"), "text/markdown; charset=utf-8", nil
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

// scorePaper 对单篇论文进行基础评分。
func scorePaper(queries []string, paper research.Paper) float64 {
	title := strings.ToLower(paper.Title)
	abstract := strings.ToLower(paper.Abstract)
	haystack := title + " " + abstract
	terms := significantQueryTerms(queries)

	score := 0.0
	for _, query := range queries {
		phrase := normalizeScoringText(query)
		if phrase == "" {
			continue
		}
		if strings.Contains(title, phrase) {
			score += 45
		} else if strings.Contains(haystack, phrase) {
			score += 18
		}
	}

	matchedTerms := 0
	for _, term := range terms {
		titleCount := strings.Count(title, term)
		abstractCount := strings.Count(abstract, term)
		if titleCount+abstractCount == 0 {
			continue
		}
		matchedTerms++
		if titleCount > 0 {
			score += 12 + math.Min(float64(titleCount-1)*2, 6)
		}
		if abstractCount > 0 {
			score += 4 + math.Min(float64(abstractCount-1)*0.5, 6)
		}
	}
	if len(terms) > 0 {
		score += float64(matchedTerms) / float64(len(terms)) * 20
		if matchedTerms == len(terms) {
			score += 8
		}
	}

	score += math.Min(math.Log10(float64(paper.CitationCount)+1)*8, 30)
	if paper.PDFURL != "" {
		score += 6
	}
	if strings.TrimSpace(paper.Abstract) != "" {
		score += 4
	}
	if paper.Year > 0 {
		age := time.Now().Year() - paper.Year
		if age >= 0 && age <= 5 {
			score += math.Max(0, 5-float64(age)*0.6)
		}
	}
	return score
}

func significantQueryTerms(queries []string) []string {
	seen := make(map[string]struct{})
	terms := make([]string, 0)
	for _, query := range queries {
		query = normalizeScoringText(query)
		for _, term := range strings.Fields(query) {
			term = strings.Trim(term, `"'.,;:!?()[]{}+-*/`)
			if len([]rune(term)) < 3 || isScoringStopWord(term) {
				continue
			}
			if _, ok := seen[term]; ok {
				continue
			}
			seen[term] = struct{}{}
			terms = append(terms, term)
		}
	}
	return terms
}

func normalizeScoringText(value string) string {
	value = strings.ToLower(value)
	value = strings.NewReplacer("-", " ", "_", " ").Replace(value)
	value = regexp.MustCompile(`\s+`).ReplaceAllString(value, " ")
	return strings.TrimSpace(value)
}

func isScoringStopWord(term string) bool {
	switch strings.ToLower(term) {
	case "and", "or", "not", "the", "for", "with", "using", "based", "study", "research", "analysis", "survey", "review":
		return true
	default:
		return false
	}
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

// extractJSON 从LLM返回的文本中提取JSON数组或对象
// 处理可能存在的markdown代码块包装
func extractJSON(text string) []byte {
	text = strings.TrimSpace(text)
	// 去除可能的markdown代码块标记
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
	}
	// 查找JSON开始位置
	start := strings.IndexAny(text, "[{")
	if start < 0 {
		return []byte(text)
	}
	// 确定JSON结束标记
	endToken := "]"
	if text[start] == '{' {
		endToken = "}"
	}
	// 查找JSON结束位置
	end := strings.LastIndex(text, endToken)
	if end <= start {
		return []byte(text[start:])
	}
	return []byte(text[start : end+1])
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
