package research

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ArxivTool Arxiv 学术预印本数据库搜索工具实现
// Arxiv 是由康奈尔大学运营的免费预印本服务器，涵盖物理、数学、计算机科学等学科
// 提供 Atom Feed 格式的 API 返回搜索结果
type ArxivTool struct {
	client *http.Client // HTTP 客户端，用于发送 API 请求
}

// NewArxivTool 创建 Arxiv 搜索工具实例
func NewArxivTool() *ArxivTool {
	// 创建 HTTP 客户端，设置 20 秒超时
	return &ArxivTool{client: &http.Client{Timeout: 20 * time.Second}}
}

// Name 返回搜索工具的名称标识
func (t *ArxivTool) Name() string {
	return "arxiv"
}

// Search 在 Arxiv 中搜索学术论文
// ctx: 上下文对象，用于控制请求超时和取消
// query: 搜索关键词
// limit: 返回结果的最大数量
// 返回: 论文列表和错误信息
func (t *ArxivTool) Search(ctx context.Context, query string, limit int) ([]Paper, error) {
	// 确保 limit 有合理的默认值
	if limit <= 0 {
		limit = 10
	}

	// 解析 Arxiv API 的基础 URL
	endpoint, err := url.Parse("https://export.arxiv.org/api/query")
	if err != nil {
		return nil, err
	}

	// 构建查询参数
	values := endpoint.Query()
	// 将用户 query 转换为 Arxiv 搜索语法
	values.Set("search_query", buildArxivSearchQuery(query))
	values.Set("start", "0")                              // 从第 0 条开始（分页偏移量）
	values.Set("max_results", fmt.Sprintf("%d", limit))   // 最大返回数量
	values.Set("sortBy", "relevance")                     // 按相关性排序
	values.Set("sortOrder", "descending")                 // 降序排列
	endpoint.RawQuery = values.Encode()

	// 创建 GET 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	// 设置 User-Agent 头，标识我们的应用
	req.Header.Set("User-Agent", "RAG-repository/1.0")

	// 发送 HTTP 请求
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 检查 HTTP 状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("arxiv returned status %s", resp.Status)
	}

	// 定义 Arxiv Atom Feed 的响应结构
	var feed arxivFeed
	// 使用 XML decoder 解析 Atom 格式响应
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, err
	}

	// 将解析结果转换为标准的 Paper 结构
	papers := make([]Paper, 0, len(feed.Entries))
	for _, entry := range feed.Entries {
		// 规范化标题文本（去除多余空白）
		title := normalizeText(entry.Title)
		if title == "" {
			continue
		}

		// 提取 PDF 下载链接
		id := strings.TrimSpace(entry.ID)
		pdfURL := ""
		for _, link := range entry.Links {
			// Arxiv 的 PDF 链接通常 title 为 "pdf" 或 type 为 "application/pdf"
			if link.Title == "pdf" || link.Type == "application/pdf" {
				pdfURL = link.Href
				break
			}
		}

		// 提取作者列表
		authors := make([]string, 0, len(entry.Authors))
		for _, author := range entry.Authors {
			if strings.TrimSpace(author.Name) != "" {
				authors = append(authors, strings.TrimSpace(author.Name))
			}
		}

		// 构建标准化的 Paper 结构
		papers = append(papers, Paper{
			Provider:   t.Name(),                    // 数据来源标识
			ExternalID: id,                           // Arxiv 论文 ID（URL 形式）
			Title:      title,                        // 论文标题
			Abstract:   normalizeText(entry.Summary), // 规范化摘要文本
			Authors:    authors,                      // 作者列表
			Year:       parseYear(entry.Published),  // 从发布日期解析年份
			URL:        id,                           // 论文页面 URL
			PDFURL:     pdfURL,                       // PDF 下载链接
		})
	}
	return papers, nil
}

// arxivFeed 表示 Arxiv API 返回的 Atom Feed 文档根结构
type arxivFeed struct {
	Entries []arxivEntry `xml:"entry"` // Feed 中的论文条目列表
}

// arxivEntry 表示 Arxiv Atom Feed 中的单个论文条目
type arxivEntry struct {
	ID        string        `xml:"id"`         // 论文的唯一标识符（URL 形式）
	Title     string        `xml:"title"`      // 论文标题
	Summary   string        `xml:"summary"`     // 论文摘要/描述
	Published string        `xml:"published"`   // 首次发布的时间
	Authors   []arxivAuthor `xml:"author"`      // 作者列表
	Links     []arxivLink   `xml:"link"`        // 相关链接（包含 PDF 链接）
}

// arxivAuthor 表示论文的作者信息
type arxivAuthor struct {
	Name string `xml:"name"` // 作者姓名
}

// arxivLink 表示论文的相关链接
// href 属性包含链接地址，type 属性描述链接类型，title 属性描述链接用途
type arxivLink struct {
	Href  string `xml:"href,attr"`  // 链接 URL
	Type  string `xml:"type,attr"`  // MIME 类型，如 "application/pdf"
	Title string `xml:"title,attr"` // 链接标题，如 "pdf"
}

// normalizeText 规范化文本，去除多余的空白字符
// 将连续空白替换为单个空格，并去除首尾空白
func normalizeText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// parseYear 从 ISO 8601 日期字符串中解析年份
// Arxiv 返回的日期格式如 "2024-01-15T10:30:00Z"
func parseYear(value string) int {
	// 日期字符串至少需要 4 个字符才能包含年份
	if len(value) < 4 {
		return 0
	}
	// 提取前 4 个字符作为年份
	year, _ := strconv.Atoi(value[:4])
	return year
}

// buildArxivSearchQuery 将用户输入的关键词转换为 Arxiv 搜索语法
// Arxiv 支持多种搜索字段：ti（标题）、au（作者）、abs（摘要）、all（全部）
func buildArxivSearchQuery(query string) string {
	// 创建字符串替换器，处理 Arxiv 查询语法中的特殊字符
	// 左括号、右括号等需要转换为空格以避免语法错误
	replacer := strings.NewReplacer("(", " ", ")", " ", "[", " ", "]", " ", "{", " ", "}", " ", "*", " ")

	// 将查询字符串分割为单词
	terms := strings.Fields(replacer.Replace(query))
	if len(terms) == 0 {
		// 如果没有有效词汇，使用 "all:" 搜索整个词
		return "all:" + query
	}

	// 构建搜索查询部分
	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		// 去除单词两端的标点符号
		term = strings.Trim(term, `"'.,;:!?`)
		// 跳过空词和 Arxiv 布尔运算符
		if term == "" || isArxivBooleanOperator(term) {
			continue
		}
		// 为每个词添加 "all:" 前缀，匹配所有字段
		parts = append(parts, "all:"+term)
		// 最多添加 8 个词，避免查询字符串过长
		if len(parts) >= 8 {
			break
		}
	}
	if len(parts) == 0 {
		return "all:" + query
	}
	// 用 AND 运算符组合所有词
	return strings.Join(parts, " AND ")
}

// isArxivBooleanOperator 判断词是否为 Arxiv 的布尔运算符
// Arxiv 支持 AND、OR、NOT 三个布尔运算符
func isArxivBooleanOperator(term string) bool {
	switch strings.ToUpper(term) {
	case "AND", "OR", "NOT":
		return true
	default:
		return false
	}
}
