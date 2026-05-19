package research

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// OpenAlexTool OpenAlex 学术数据库搜索工具实现
// OpenAlex 是由 Our Research 开发的免费学术论文元数据数据库
// 收录了全球学术论文的元数据、引用关系、开放获取状态等信息
// API 默认只返回开放获取（Open Access）论文
type OpenAlexTool struct {
	client *http.Client // HTTP 客户端，用于发送 API 请求
}

// NewOpenAlexTool 创建 OpenAlex 搜索工具实例
func NewOpenAlexTool() *OpenAlexTool {
	// 创建 HTTP 客户端，设置 20 秒超时
	return &OpenAlexTool{client: &http.Client{Timeout: 20 * time.Second}}
}

// Name 返回搜索工具的名称标识
func (t *OpenAlexTool) Name() string {
	return "openalex"
}

// Search 在 OpenAlex 中搜索学术论文
// ctx: 上下文对象，用于控制请求超时和取消
// query: 搜索关键词
// limit: 返回结果的最大数量
// 返回: 论文列表和错误信息
func (t *OpenAlexTool) Search(ctx context.Context, query string, limit int) ([]Paper, error) {
	// 确保 limit 有合理的默认值
	if limit <= 0 {
		limit = 10
	}

	// 解析 OpenAlex API 的基础 URL
	endpoint, err := url.Parse("https://api.openalex.org/works")
	if err != nil {
		return nil, err
	}

	// 构建查询参数
	values := endpoint.Query()
	values.Set("search", query)                              // 搜索关键词
	values.Set("per-page", fmt.Sprintf("%d", limit))       // 每页数量
	values.Set("filter", "is_oa:true")                     // 只返回开放获取论文
	values.Set("sort", "relevance_score:desc")             // 按相关性降序排列
	endpoint.RawQuery = values.Encode()

	// 创建 GET 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	// 设置 User-Agent 头，标识我们的应用
	req.Header.Set("User-Agent", "RAG-repository/1.0 (mailto:example@example.com)")

	// 发送 HTTP 请求
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 检查 HTTP 状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openalex returned status %s", resp.Status)
	}

	// 定义响应体结构体，与 OpenAlex API 的 JSON 格式匹配
	var body struct {
		Results []struct {
			ID                    string           `json:"id"`                      // OpenAlex 内部 ID
			DOI                   string           `json:"doi"`                     // DOI
			DisplayName           string           `json:"display_name"`            // 论文标题
			PublicationYear       int              `json:"publication_year"`        // 发表年份
			CitedByCount          int              `json:"cited_by_count"`          // 被引用次数
			// AbstractInvertedIndex 摘要的反向索引，需要重建才能得到完整摘要
			// 格式为 {"word": [positions]}，表示词在哪些位置出现
			AbstractInvertedIndex map[string][]int `json:"abstract_inverted_index"`
			// Authorships 作者 affiliation 信息
			Authorships []struct {
				Author struct {
					DisplayName string `json:"display_name"` // 作者姓名
				} `json:"author"`
			} `json:"authorships"`
			// PrimaryLocation 论文的主要位置信息
			PrimaryLocation *struct {
				LandingPageURL string `json:"landing_page_url"` // 论文页面 URL
				PDFURL         string `json:"pdf_url"`          // PDF 链接
			} `json:"primary_location"`
			// OpenAccess 开放获取状态
			OpenAccess *struct {
				OAURL string `json:"oa_url"` // 开放获取 URL
			} `json:"open_access"`
		} `json:"results"`
	}

	// 解析 JSON 响应体
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	// 将解析结果转换为标准的 Paper 结构
	papers := make([]Paper, 0, len(body.Results))
	for _, item := range body.Results {
		// 规范化标题文本
		title := normalizeText(item.DisplayName)
		// 优先使用 DOI，其次使用 OpenAlex ID
		id := strings.TrimSpace(firstNonEmpty([]string{item.DOI, item.ID}))
		if title == "" || id == "" {
			continue
		}

		// 提取作者列表
		authors := make([]string, 0, len(item.Authorships))
		for _, authorship := range item.Authorships {
			name := normalizeText(authorship.Author.DisplayName)
			if name != "" {
				authors = append(authors, name)
			}
		}

		// 提取 URL 和 PDF 链接
		urlValue := ""
		pdfURL := ""
		if item.PrimaryLocation != nil {
			urlValue = item.PrimaryLocation.LandingPageURL
			pdfURL = item.PrimaryLocation.PDFURL
		}
		// 如果没有 landing page URL，使用 OpenAlex ID
		if urlValue == "" {
			urlValue = item.ID
		}

		// 构建标准化的 Paper 结构
		papers = append(papers, Paper{
			Provider:      t.Name(), // 数据来源标识
			ExternalID:    id,       // DOI 或 OpenAlex ID
			Title:         title,   // 论文标题
			Abstract:      reconstructOpenAlexAbstract(item.AbstractInvertedIndex), // 重建的摘要
			Authors:       authors, // 作者列表
			Year:          item.PublicationYear, // 发表年份
			URL:           urlValue,             // 论文页面 URL
			PDFURL:        pdfURL,               // PDF 链接
			CitationCount: item.CitedByCount,    // 被引用次数
		})
	}
	return papers, nil
}

// reconstructOpenAlexAbstract 从反向索引重建摘要文本
// OpenAlex 为了节省空间，将摘要存储为反向索引格式
// 格式为 {"word": [positions]}，需要根据位置重新组装成完整文本
func reconstructOpenAlexAbstract(index map[string][]int) string {
	// 如果没有摘要索引，返回空字符串
	if len(index) == 0 {
		return ""
	}

	// positionedWord 将词及其位置组合在一起
	type positionedWord struct {
		word     string
		position int
	}

	// 将反向索引转换为词+位置的列表
	words := make([]positionedWord, 0)
	for word, positions := range index {
		for _, position := range positions {
			words = append(words, positionedWord{word: word, position: position})
		}
	}

	// 按位置排序，使词按原始顺序排列
	sort.Slice(words, func(i, j int) bool {
		return words[i].position < words[j].position
	})

	// 提取排序后的词列表
	ordered := make([]string, 0, len(words))
	for _, item := range words {
		ordered = append(ordered, item.word)
	}

	// 组合成完整摘要并规范化
	return normalizeText(strings.Join(ordered, " "))
}
