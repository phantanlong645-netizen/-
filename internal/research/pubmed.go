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

type PubMedTool struct {
	client *http.Client
	mu     sync.Mutex
	last   time.Time
}

func NewPubMedTool() *PubMedTool {
	return &PubMedTool{client: &http.Client{Timeout: 20 * time.Second}}
}

func (t *PubMedTool) Name() string {
	return "pubmed"
}

func (t *PubMedTool) Search(ctx context.Context, query string, limit int) ([]Paper, error) {
	if limit <= 0 {
		limit = 10
	}

	ids, err := t.searchIDs(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	return t.fetchArticles(ctx, ids)
}

func (t *PubMedTool) searchIDs(ctx context.Context, query string, limit int) ([]string, error) {
	endpoint, err := url.Parse("https://eutils.ncbi.nlm.nih.gov/entrez/eutils/esearch.fcgi")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("db", "pubmed")
	values.Set("term", query)
	values.Set("retmode", "json")
	values.Set("retmax", fmt.Sprintf("%d", limit))
	values.Set("sort", "relevance")
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "RAG-repository/1.0")

	resp, err := t.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pubmed esearch returned status %s", resp.Status)
	}

	var body struct {
		ESearchResult struct {
			IDList []string `json:"idlist"`
		} `json:"esearchresult"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.ESearchResult.IDList, nil
}

func (t *PubMedTool) fetchArticles(ctx context.Context, ids []string) ([]Paper, error) {
	endpoint, err := url.Parse("https://eutils.ncbi.nlm.nih.gov/entrez/eutils/efetch.fcgi")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("db", "pubmed")
	values.Set("id", strings.Join(ids, ","))
	values.Set("retmode", "xml")
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "RAG-repository/1.0")

	resp, err := t.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pubmed efetch returned status %s", resp.Status)
	}

	var set pubmedArticleSet
	if err := xml.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}

	papers := make([]Paper, 0, len(set.Articles))
	for _, article := range set.Articles {
		pmid := strings.TrimSpace(article.MedlineCitation.PMID.Value)
		title := normalizeText(html.UnescapeString(article.MedlineCitation.Article.ArticleTitle))
		if pmid == "" || title == "" {
			continue
		}

		papers = append(papers, Paper{
			Provider:   t.Name(),
			ExternalID: pmid,
			Title:      title,
			Abstract:   article.AbstractText(),
			Authors:    article.Authors(),
			Year:       article.Year(),
			URL:        "https://pubmed.ncbi.nlm.nih.gov/" + pmid + "/",
			PDFURL:     article.PDFURL(),
		})
	}
	return papers, nil
}

func (t *PubMedTool) do(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.last.IsZero() {
		wait := 350*time.Millisecond - time.Since(t.last)
		if wait > 0 {
			time.Sleep(wait)
		}
	}
	t.last = time.Now()
	return t.client.Do(req)
}

type pubmedArticleSet struct {
	Articles []pubmedArticle `xml:"PubmedArticle"`
}

type pubmedArticle struct {
	MedlineCitation struct {
		PMID struct {
			Value string `xml:",chardata"`
		} `xml:"PMID"`
		Article struct {
			ArticleTitle string `xml:"ArticleTitle"`
			Abstract     struct {
				Texts []struct {
					Label string `xml:"Label,attr"`
					Value string `xml:",chardata"`
				} `xml:"AbstractText"`
			} `xml:"Abstract"`
			Journal struct {
				JournalIssue struct {
					PubDate struct {
						Year        string `xml:"Year"`
						MedlineDate string `xml:"MedlineDate"`
					} `xml:"PubDate"`
				} `xml:"JournalIssue"`
			} `xml:"Journal"`
			AuthorList struct {
				Authors []struct {
					LastName       string `xml:"LastName"`
					ForeName       string `xml:"ForeName"`
					CollectiveName string `xml:"CollectiveName"`
				} `xml:"Author"`
			} `xml:"AuthorList"`
			ELocationIDs []struct {
				EIDType string `xml:"EIdType,attr"`
				Value   string `xml:",chardata"`
			} `xml:"ELocationID"`
		} `xml:"Article"`
	} `xml:"MedlineCitation"`
	PubmedData struct {
		ArticleIDList struct {
			ArticleIDs []struct {
				IDType string `xml:"IdType,attr"`
				Value  string `xml:",chardata"`
			} `xml:"ArticleId"`
		} `xml:"ArticleIdList"`
	} `xml:"PubmedData"`
}

func (a pubmedArticle) AbstractText() string {
	parts := make([]string, 0, len(a.MedlineCitation.Article.Abstract.Texts))
	for _, item := range a.MedlineCitation.Article.Abstract.Texts {
		text := normalizeText(html.UnescapeString(item.Value))
		if text == "" {
			continue
		}
		label := normalizeText(item.Label)
		if label != "" {
			text = label + ": " + text
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " ")
}

func (a pubmedArticle) Authors() []string {
	authors := make([]string, 0, len(a.MedlineCitation.Article.AuthorList.Authors))
	for _, author := range a.MedlineCitation.Article.AuthorList.Authors {
		name := normalizeText(strings.TrimSpace(author.ForeName + " " + author.LastName))
		if name == "" {
			name = normalizeText(author.CollectiveName)
		}
		if name != "" {
			authors = append(authors, name)
		}
	}
	return authors
}

func (a pubmedArticle) Year() int {
	year, _ := strconv.Atoi(a.MedlineCitation.Article.Journal.JournalIssue.PubDate.Year)
	if year > 0 {
		return year
	}

	medlineDate := a.MedlineCitation.Article.Journal.JournalIssue.PubDate.MedlineDate
	if len(medlineDate) >= 4 {
		year, _ = strconv.Atoi(medlineDate[:4])
	}
	return year
}

func (a pubmedArticle) PDFURL() string {
	for _, id := range a.PubmedData.ArticleIDList.ArticleIDs {
		if strings.EqualFold(id.IDType, "pmc") && strings.TrimSpace(id.Value) != "" {
			return "https://www.ncbi.nlm.nih.gov/pmc/articles/" + strings.TrimSpace(id.Value) + "/pdf/"
		}
	}
	return ""
}
