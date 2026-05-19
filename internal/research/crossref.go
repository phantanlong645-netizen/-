package research

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// CrossrefTool Crossref 学术数据库搜索工具实现
// Crossref 是由 Crossref 运营的学术论文元数据数据库
// 收录了全球数万出版社的学术论文元数据，包括 DOI、作者、引用等信息
type CrossrefTool struct {
	client *http.Client // HTTP 客户端，用于发送 API 请求
}

// NewCrossrefTool 创建 Crossref 搜索工具实例
func NewCrossrefTool() *CrossrefTool {
	// 创建 HTTP 客户端，设置 20 秒超时
	return &CrossrefTool{client: &http.Client{Timeout: 20 * time.Second}}
}

// Name 返回搜索工具的名称标识
func (t *CrossrefTool) Name() string {
	return "crossref"
}

// Search 在 Crossref 中搜索学术论文
// ctx: 上下文对象，用于控制请求超时和取消
// query: 搜索关键词
// limit: 返回结果的最大数量
// 返回: 论文列表和错误信息
func (t *CrossrefTool) Search(ctx context.Context, query string, limit int) ([]Paper, error) {
	// 确保 limit 有合理的默认值
	if limit <= 0 {
		limit = 10
	}

	// 解析 Crossref API 的基础 URL
	endpoint, err := url.Parse("https://api.crossref.org/works")
	if err != nil {
		return nil, err
	}

	// 构建查询参数
	values := endpoint.Query()
	values.Set("query", query)   // 搜索关键词
	values.Set("rows", fmt.Sprintf("%d", limit)) // 返回数量
	// select 指定返回的字段，减少不必要的数据传输
	values.Set("select", "DOI,title,abstract,published-print,published-online,published,URL,author,is-referenced-by-count,link")
	endpoint.RawQuery = values.Encode()

	// 创建 GET 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	// Crossref API 要求设置 User-Agent 头，最好包含联系方式
	req.Header.Set("User-Agent", "RAG-repository/1.0 (mailto:example@example.com)")

	// 发送 HTTP 请求
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 检查 HTTP 状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("crossref returned status %s", resp.Status)
	}

	// 定义响应体结构体，与 Crossref API 的 JSON 格式匹配
	var body struct {
		Message struct {
			Items []struct {
				DOI                 string   `json:"DOI"`                  // 数字对象标识符
				Title               []string `json:"title"`                // 标题数组（通常只有一个元素）
				Abstract            string   `json:"abstract"`             // 摘要（可能包含 HTML 标签）
				URL                 string   `json:"URL"`                  // 论文页面 URL
				IsReferencedByCount int      `json:"is-referenced-by-count"` // 被引用次数
				// Author 作者列表
				Author []struct {
					Given  string `json:"given"`  // 名
					Family string `json:"family"` // 姓
				} `json:"author"`
				// 日期字段，可能有 print、online、general 三种
				PublishedPrint  crossrefDate `json:"published-print"`
				PublishedOnline crossrefDate `json:"published-online"`
				Published       crossrefDate `json:"published"`
				// Link 链接列表，可能包含 PDF 链接
				Link []struct {
					URL         string `json:"URL"`          // 链接地址
					ContentType string `json:"content-type"` // 内容类型，如 "application/pdf"
				} `json:"link"`
			} `json:"items"`
		} `json:"message"`
	}

	// 解析 JSON 响应体
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	// 将解析结果转换为标准的 Paper 结构
	papers := make([]Paper, 0, len(body.Message.Items))
	for _, item := range body.Message.Items {
		// 提取第一个非空标题
		title := firstNonEmpty(item.Title)
		// DOI 是必需的唯一标识符
		doi := strings.TrimSpace(item.DOI)
		if title == "" || doi == "" {
			continue
		}

		// 构造作者全名列表
		authors := make([]string, 0, len(item.Author))
		for _, author := range item.Author {
			// 组合 Given 和 Family 姓名
			name := strings.TrimSpace(strings.TrimSpace(author.Given) + " " + strings.TrimSpace(author.Family))
			if name != "" {
				authors = append(authors, name)
			}
		}

		// 查找 PDF 链接
		pdfURL := ""
		for _, link := range item.Link {
			// 根据 content-type 判断是否为 PDF 链接
			if strings.Contains(strings.ToLower(link.ContentType), "pdf") {
				pdfURL = link.URL
				break
			}
		}

		// 构建标准化的 Paper 结构
		papers = append(papers, Paper{
			Provider:      t.Name(), // 数据来源标识
			ExternalID:    doi,      // DOI 作为外部唯一标识
			Title:         title,    // 论文标题
			Abstract:      stripHTML(item.Abstract), // 去除 HTML 标签后的摘要
			Authors:       authors,  // 作者列表
			Year:          firstYear(item.PublishedPrint, item.PublishedOnline, item.Published), // 优先使用 Print 日期
			URL:           item.URL,
			PDFURL:        pdfURL,
			CitationCount: item.IsReferencedByCount, // 被引用次数
		})
	}
	return papers, nil
}

// crossrefDate 表示 Crossref 中的日期结构
// date-parts 是嵌套数组，如 [[2024, 1, 15]] 表示 2024 年 1 月 15 日
type crossrefDate struct {
	DateParts [][]int `json:"date-parts"`
}

// firstYear 尝试从多个日期中获取第一个有效的年份
// 优先使用 PublishedPrint，其次 PublishedOnline，最后 Published
func firstYear(dates ...crossrefDate) int {
	for _, date := range dates {
		// 检查是否有日期数据
		if len(date.DateParts) > 0 && len(date.DateParts[0]) > 0 {
			return date.DateParts[0][0] // 返回年份（第一个元素）
		}
	}
	return 0
}

// firstNonEmpty 返回字符串数组中的第一个非空字符串
// 用于处理 Crossref 中 Title 是数组的情况
func firstNonEmpty(values []string) string {
	for _, value := range values {
		value = normalizeText(value)
		if value != "" {
			return value
		}
	}
	return ""
}

// stripHTML 去除 HTML 标签
// Crossref 的摘要字段可能包含 HTML 标签，需要去除
func stripHTML(value string) string {
	// 使用正则表达式匹配并替换 HTML 标签为空格
	value = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(value, " ")
	return normalizeText(value)
}
