package research

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SemanticScholarTool Semantic Scholar 学术数据库搜索工具实现
// Semantic Scholar 是由 Allen Institute for AI 开发的免费学术搜索引擎
// 提供了丰富的论文元数据、引用关系和开放获取 PDF 链接
type SemanticScholarTool struct {
	apiKey string      // Semantic Scholar API 密钥，可选，有密钥可获得更高调用限额
	client *http.Client // HTTP 客户端，用于发送 API 请求
}

// NewSemanticScholarTool 创建 Semantic Scholar 搜索工具实例
// apiKey: 可选的 API 密钥，如有可提高请求频率限制
func NewSemanticScholarTool(apiKey string) *SemanticScholarTool {
	return &SemanticScholarTool{
		apiKey: apiKey,
		// 创建 HTTP 客户端，设置 20 秒超时避免请求阻塞
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

// Name 返回搜索工具的名称标识
// 用于在日志中标识信息来源，以及在结果去重时作为 key 的一部分
func (t *SemanticScholarTool) Name() string {
	return "semantic_scholar"
}

// Search 在 Semantic Scholar 中搜索学术论文
// ctx: 上下文对象，用于控制请求超时和取消
// query: 搜索关键词
// limit: 返回结果的最大数量
// 返回: 论文列表和错误信息
func (t *SemanticScholarTool) Search(ctx context.Context, query string, limit int) ([]Paper, error) {
	// 确保 limit 有合理的默认值，避免无效请求
	if limit <= 0 {
		limit = 10
	}

	// 解析 Semantic Scholar API 的基础 URL
	endpoint, err := url.Parse("https://api.semanticscholar.org/graph/v1/paper/search")
	if err != nil {
		return nil, err
	}

	// 构建查询参数
	values := endpoint.Query()
	values.Set("query", query)                                                    // 搜索关键词
	values.Set("limit", fmt.Sprintf("%d", limit))                                // 返回数量限制
	// 指定返回的字段，减少不必要的数据传输
	values.Set("fields", "paperId,title,abstract,year,url,authors,citationCount,openAccessPdf")
	endpoint.RawQuery = values.Encode()

	// 创建 GET 请求，将查询参数附加到 URL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}

	// 如果提供了 API 密钥，在请求头中添加认证信息
	if t.apiKey != "" {
		req.Header.Set("x-api-key", t.apiKey)
	}

	// 发送 HTTP 请求
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	// 确保响应体被关闭，释放网络连接
	defer resp.Body.Close()

	// 检查 HTTP 状态码，非 200 表示请求失败
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("semantic scholar returned status %s", resp.Status)
	}

	// 定义响应体结构体，与 Semantic Scholar API 的 JSON 格式匹配
	var body struct {
		// Data 数组包含搜索到的论文列表
		Data []struct {
			PaperID       string `json:"paperId"`       // Semantic Scholar 内部论文 ID
			Title         string `json:"title"`         // 论文标题
			Abstract      string `json:"abstract"`      // 论文摘要
			Year          int    `json:"year"`          // 发表年份
			URL           string `json:"url"`           // 论文页面 URL
			CitationCount int    `json:"citationCount"` // 引用次数
			// OpenAccessPDF 包含开放获取 PDF 的下载链接
			OpenAccessPDF *struct {
				URL string `json:"url"` // PDF 直接下载链接
			} `json:"openAccessPdf"`
			Authors []struct {
				Name string `json:"name"` // 作者姓名
			} `json:"authors"`
		} `json:"data"`
	}

	// 解析 JSON 响应体
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	// 将 API 返回的数据转换为标准的 Paper 结构
	papers := make([]Paper, 0, len(body.Data))
	for _, item := range body.Data {
		// 跳过无效的论文记录（标题或 ID 为空）
		if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.PaperID) == "" {
			continue
		}

		// 提取作者列表，过滤空名字
		authors := make([]string, 0, len(item.Authors))
		for _, author := range item.Authors {
			if strings.TrimSpace(author.Name) != "" {
				authors = append(authors, author.Name)
			}
		}

		// 提取 PDF 下载链接（如果有开放获取版本）
		pdfURL := ""
		if item.OpenAccessPDF != nil {
			pdfURL = item.OpenAccessPDF.URL
		}

		// 构建标准化的 Paper 结构
		papers = append(papers, Paper{
			Provider:      t.Name(),        // 数据来源标识
			ExternalID:    item.PaperID,     // Semantic Scholar 论文 ID
			Title:         item.Title,      // 论文标题
			Abstract:      item.Abstract,   // 论文摘要
			Authors:       authors,         // 作者列表
			Year:          item.Year,      // 发表年份
			URL:           item.URL,       // 论文页面 URL
			PDFURL:        pdfURL,          // PDF 下载链接
			CitationCount: item.CitationCount, // 引用次数
		})
	}
	return papers, nil
}
