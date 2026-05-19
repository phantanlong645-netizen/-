package research

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PubMedTool PubMed 医学文献数据库搜索工具实现
// PubMed 是由美国国家医学图书馆（NLM）运营的免费医学文献检索系统
// 收录了生物医学和生命科学领域的学术论文
// API 使用两步查询：先搜索获取 ID 列表，再根据 ID 获取详细信息
type PubMedTool struct {
	client *http.Client // HTTP 客户端，用于发送 API 请求
	mu     sync.Mutex   // 互斥锁，用于实现请求频率限制
	last   time.Time    // 上次请求时间，用于控制请求间隔
}

// NewPubMedTool 创建 PubMed 搜索工具实例
func NewPubMedTool() *PubMedTool {
	return &PubMedTool{client: &http.Client{Timeout: 20 * time.Second}}
}

// Name 返回搜索工具的名称标识
func (t *PubMedTool) Name() string {
	return "pubmed"
}

// Search 在 PubMed 中搜索医学文献
// ctx: 上下文对象，用于控制请求超时和取消
// query: 搜索关键词
// limit: 返回结果的最大数量
// 返回: 论文列表和错误信息
func (t *PubMedTool) Search(ctx context.Context, query string, limit int) ([]Paper, error) {
	// 确保 limit 有合理的默认值
	if limit <= 0 {
		limit = 10
	}

	// 第一步：使用 ESearch API 根据关键词搜索，获取匹配的论文 ID 列表
	ids, err := t.searchIDs(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		// 如果没有匹配的论文，返回空列表
		return nil, nil
	}

	// 第二步：使用 EFetch API 根据 ID 列表获取论文的详细信息
	return t.fetchArticles(ctx, ids)
}

// searchIDs 在 PubMed 中搜索并获取匹配的论文 ID 列表
// ESearch API 是 PubMed 的搜索接口，返回匹配的 PubMed ID (PMID) 列表
func (t *PubMedTool) searchIDs(ctx context.Context, query string, limit int) ([]string, error) {
	// 解析 ESearch API 的 URL
	endpoint, err := url.Parse("https://eutils.ncbi.nlm.nih.gov/entrez/eutils/esearch.fcgi")
	if err != nil {
		return nil, err
	}

	// 构建查询参数
	values := endpoint.Query()
	values.Set("db", "pubmed")                              // 指定数据库为 PubMed
	values.Set("term", query)                              // 搜索关键词
	values.Set("retmode", "json")                           // 返回 JSON 格式
	values.Set("retmax", fmt.Sprintf("%d", limit))         // 最大返回数量
	values.Set("sort", "relevance")                        // 按相关性排序
	endpoint.RawQuery = values.Encode()

	// 创建 GET 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "RAG-repository/1.0")

	// 发送请求并处理响应
	resp, err := t.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 检查 HTTP 状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pubmed esearch returned status %s", resp.Status)
	}

	// 定义响应结构体，与 ESearch JSON 格式匹配
	var body struct {
		ESearchResult struct {
			IDList []string `json:"idlist"` // PubMed ID 列表
		} `json:"esearchresult"`
	}

	// 解析 JSON 响应
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.ESearchResult.IDList, nil
}

// fetchArticles 根据论文 ID 列表获取详细的论文信息
// EFetch API 是 PubMed 的详情获取接口，返回论文的 XML 格式详细信息
func (t *PubMedTool) fetchArticles(ctx context.Context, ids []string) ([]Paper, error) {
	// 解析 EFetch API 的 URL
	endpoint, err := url.Parse("https://eutils.ncbi.nlm.nih.gov/entrez/eutils/efetch.fcgi")
	if err != nil {
		return nil, err
	}

	// 构建查询参数
	values := endpoint.Query()
	values.Set("db", "pubmed")                          // 指定数据库为 PubMed
	values.Set("id", strings.Join(ids, ","))          // 逗号分隔的 ID 列表
	values.Set("retmode", "xml")                       // 返回 XML 格式
	endpoint.RawQuery = values.Encode()

	// 创建 GET 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "RAG-repository/1.0")

	// 发送请求并处理响应
	resp, err := t.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 检查 HTTP 状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pubmed efetch returned status %s", resp.Status)
	}

	// 定义 XML 响应结构体
	var set pubmedArticleSet
	// 使用 XML decoder 解析响应
	if err := xml.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}

	// 将解析结果转换为标准的 Paper 结构
	papers := make([]Paper, 0, len(set.Articles))
	for _, article := range set.Articles {
		// 提取 PubMed ID
		pmid := strings.TrimSpace(article.MedlineCitation.PMID.Value)
		// 提取并规范化标题
		title := normalizeText(html.UnescapeString(article.MedlineCitation.Article.ArticleTitle))
		if pmid == "" || title == "" {
			continue
		}

		// 构建标准化的 Paper 结构
		papers = append(papers, Paper{
			Provider:   t.Name(),                  // 数据来源标识
			ExternalID: pmid,                      // PubMed ID
			Title:      title,                     // 论文标题
			Abstract:   article.AbstractText(),    // 论文摘要
			Authors:    article.Authors(),          // 作者列表
			Year:       article.Year(),            // 发表年份
			URL:        "https://pubmed.ncbi.nlm.nih.gov/" + pmid + "/", // PubMed 页面 URL
			PDFURL:     article.PDFURL(),          // PDF 链接
		})
	}
	return papers, nil
}

// do 发送 HTTP 请求并实现请求频率限制
// PubMed API 要求每秒请求不超过 1 次（350ms 间隔）
// 使用互斥锁确保线程安全，使用 time.Sleep 实现请求间隔
func (t *PubMedTool) do(req *http.Request) (*http.Response, error) {
	t.mu.Lock()         // 获取互斥锁
	defer t.mu.Unlock() // 函数返回时释放锁

	// 计算距离上次请求经过的时间
	if !t.last.IsZero() {
		// 计算需要等待的时间（API 要求 350ms 间隔）
		wait := 350*time.Millisecond - time.Since(t.last)
		if wait > 0 {
			time.Sleep(wait) // 等待以满足频率限制
		}
	}
	// 更新最后请求时间
	t.last = time.Now()
	return t.client.Do(req)
}

// pubmedArticleSet 表示 PubMed XML 响应中的文章集合
type pubmedArticleSet struct {
	Articles []pubmedArticle `xml:"PubmedArticle"` // PubMed 文章列表
}

// pubmedArticle 表示单篇 PubMed 文章的 XML 结构
type pubmedArticle struct {
	MedlineCitation struct {
		PMID struct {
			Value string `xml:",chardata"` // PubMed ID 的文本值
		} `xml:"PMID"`
		Article struct {
			ArticleTitle string `xml:"ArticleTitle"` // 论文标题
			// Abstract 包含摘要文本，可能有多个部分（不同标签）
			Abstract struct {
				Texts []struct {
					Label string `xml:"Label,attr"` // 标签，如 "BACKGROUND"、"METHODS" 等
					Value string `xml:",chardata"`  // 摘要文本内容
				} `xml:"AbstractText"`
			} `xml:"Abstract"`
			// Journal 期刊信息
			Journal struct {
				JournalIssue struct {
					PubDate struct {
						Year        string `xml:"Year"`        // 年份
						MedlineDate string `xml:"MedlineDate"` // 完整日期字符串
					} `xml:"PubDate"`
				} `xml:"JournalIssue"`
			} `xml:"Journal"`
			// AuthorList 作者列表
			AuthorList struct {
				Authors []struct {
					LastName       string `xml:"LastName"`        // 姓
					ForeName       string `xml:"ForeName"`        // 名
					CollectiveName string `xml:"CollectiveName"`  // 集体作者名称（如大型合作组的名称）
				} `xml:"Author"`
			} `xml:"AuthorList"`
			// ELocationIDs 电子位置标识符，可能包含 DOI
			ELocationIDs []struct {
				EIDType string `xml:"EIdType,attr"` // 标识符类型，如 "doi"
				Value   string `xml:",chardata"`   // 标识符值
			} `xml:"ELocationID"`
		} `xml:"Article"`
	} `xml:"MedlineCitation"`
	PubmedData struct {
		ArticleIDList struct {
			ArticleIDs []struct {
				IDType string `xml:"IdType,attr"` // ID 类型，如 "pubmed"、"doi"
				Value  string `xml:",chardata"`   // ID 值
			} `xml:"ArticleId"`
		} `xml:"ArticleIdList"`
	} `xml:"PubmedData"`
}

// AbstractText 组合论文的摘要文本
// PubMed 的摘要可能分为多个部分（用 Label 标识），此方法将它们组合成单个字符串
func (a *pubmedArticle) AbstractText() string {
	if len(a.MedlineCitation.Article.Abstract.Texts) == 0 {
		return ""
	}
	var parts []string
	for _, text := range a.MedlineCitation.Article.Abstract.Texts {
		parts = append(parts, text.Value)
	}
	return normalizeText(strings.Join(parts, " "))
}

// Authors 提取论文的作者列表
// 优先使用个人作者姓名（LastName + ForeName），其次使用集体作者名称
func (a *pubmedArticle) Authors() []string {
	var authors []string
	for _, author := range a.MedlineCitation.Article.AuthorList.Authors {
		if author.LastName != "" && author.ForeName != "" {
			// 个人作者：组合姓和名
			authors = append(authors, normalizeText(author.LastName+" "+author.ForeName))
		} else if author.CollectiveName != "" {
			// 集体作者：使用集体名称
			authors = append(authors, normalizeText(author.CollectiveName))
		}
	}
	return authors
}

// Year 提取论文的发表年份
// 优先尝试解析 Year 字段，如果失败则解析 MedlineDate 字符串
func (a *pubmedArticle) Year() int {
	yearStr := a.MedlineCitation.Article.Journal.JournalIssue.PubDate.Year
	if yearStr != "" {
		if year, err := strconv.Atoi(yearStr); err == nil {
			return year
		}
	}
	// 如果 Year 为空，尝试从 MedlineDate 解析
	// MedlineDate 格式如 "2024 Jan 15"
	medlineDate := a.MedlineCitation.Article.Journal.JournalIssue.PubDate.MedlineDate
	if len(medlineDate) >= 4 {
		if year, err := strconv.Atoi(medlineDate[:4]); err == nil {
			return year
		}
	}
	return 0
}

// PDFURL 尝试获取论文的 PDF 下载链接
// 从 ELocationIDs 中查找 DOI，然后构造 PubMed Central (PMC) 的 PDF URL
func (a *pubmedArticle) PDFURL() string {
	// 查找 DOI
	var doi string
	for _, eid := range a.MedlineCitation.Article.ELocationIDs {
		if strings.ToLower(eid.EIDType) == "doi" {
			doi = eid.Value
			break
		}
	}
	if doi == "" {
		return ""
	}
	// 尝试构造 PMC PDF URL（如果论文在 PMC 有免费全文）
	// 注意：这只适用于 PMC 收录的论文，不是所有论文都有 PMC 版本
	return "https://www.ncbi.nlm.nih.gov/pmc/articles/" + doi + "/pdf/"
}
