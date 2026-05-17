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

type CrossrefTool struct {
	client *http.Client
}

func NewCrossrefTool() *CrossrefTool {
	return &CrossrefTool{client: &http.Client{Timeout: 20 * time.Second}}
}

func (t *CrossrefTool) Name() string {
	return "crossref"
}

func (t *CrossrefTool) Search(ctx context.Context, query string, limit int) ([]Paper, error) {
	if limit <= 0 {
		limit = 10
	}

	endpoint, err := url.Parse("https://api.crossref.org/works")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("query", query)
	values.Set("rows", fmt.Sprintf("%d", limit))
	values.Set("select", "DOI,title,abstract,published-print,published-online,published,URL,author,is-referenced-by-count,link")
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "RAG-repository/1.0 (mailto:example@example.com)")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("crossref returned status %s", resp.Status)
	}

	var body struct {
		Message struct {
			Items []struct {
				DOI                 string   `json:"DOI"`
				Title               []string `json:"title"`
				Abstract            string   `json:"abstract"`
				URL                 string   `json:"URL"`
				IsReferencedByCount int      `json:"is-referenced-by-count"`
				Author              []struct {
					Given  string `json:"given"`
					Family string `json:"family"`
				} `json:"author"`
				PublishedPrint  crossrefDate `json:"published-print"`
				PublishedOnline crossrefDate `json:"published-online"`
				Published       crossrefDate `json:"published"`
				Link            []struct {
					URL         string `json:"URL"`
					ContentType string `json:"content-type"`
				} `json:"link"`
			} `json:"items"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	papers := make([]Paper, 0, len(body.Message.Items))
	for _, item := range body.Message.Items {
		title := firstNonEmpty(item.Title)
		doi := strings.TrimSpace(item.DOI)
		if title == "" || doi == "" {
			continue
		}

		authors := make([]string, 0, len(item.Author))
		for _, author := range item.Author {
			name := strings.TrimSpace(strings.TrimSpace(author.Given) + " " + strings.TrimSpace(author.Family))
			if name != "" {
				authors = append(authors, name)
			}
		}

		pdfURL := ""
		for _, link := range item.Link {
			if strings.Contains(strings.ToLower(link.ContentType), "pdf") {
				pdfURL = link.URL
				break
			}
		}

		papers = append(papers, Paper{
			Provider:      t.Name(),
			ExternalID:    doi,
			Title:         title,
			Abstract:      stripHTML(item.Abstract),
			Authors:       authors,
			Year:          firstYear(item.PublishedPrint, item.PublishedOnline, item.Published),
			URL:           item.URL,
			PDFURL:        pdfURL,
			CitationCount: item.IsReferencedByCount,
		})
	}
	return papers, nil
}

type crossrefDate struct {
	DateParts [][]int `json:"date-parts"`
}

func firstYear(dates ...crossrefDate) int {
	for _, date := range dates {
		if len(date.DateParts) > 0 && len(date.DateParts[0]) > 0 {
			return date.DateParts[0][0]
		}
	}
	return 0
}

func firstNonEmpty(values []string) string {
	for _, value := range values {
		value = normalizeText(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func stripHTML(value string) string {
	value = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(value, " ")
	return normalizeText(value)
}
