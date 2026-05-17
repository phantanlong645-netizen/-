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

type SemanticScholarTool struct {
	apiKey string
	client *http.Client
}

func NewSemanticScholarTool(apiKey string) *SemanticScholarTool {
	return &SemanticScholarTool{
		apiKey: apiKey,
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

func (t *SemanticScholarTool) Name() string {
	return "semantic_scholar"
}

func (t *SemanticScholarTool) Search(ctx context.Context, query string, limit int) ([]Paper, error) {
	if limit <= 0 {
		limit = 10
	}

	endpoint, err := url.Parse("https://api.semanticscholar.org/graph/v1/paper/search")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("query", query)
	values.Set("limit", fmt.Sprintf("%d", limit))
	values.Set("fields", "paperId,title,abstract,year,url,authors,citationCount,openAccessPdf")
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	if t.apiKey != "" {
		req.Header.Set("x-api-key", t.apiKey)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("semantic scholar returned status %s", resp.Status)
	}

	var body struct {
		Data []struct {
			PaperID       string `json:"paperId"`
			Title         string `json:"title"`
			Abstract      string `json:"abstract"`
			Year          int    `json:"year"`
			URL           string `json:"url"`
			CitationCount int    `json:"citationCount"`
			OpenAccessPDF *struct {
				URL string `json:"url"`
			} `json:"openAccessPdf"`
			Authors []struct {
				Name string `json:"name"`
			} `json:"authors"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	papers := make([]Paper, 0, len(body.Data))
	for _, item := range body.Data {
		if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.PaperID) == "" {
			continue
		}
		authors := make([]string, 0, len(item.Authors))
		for _, author := range item.Authors {
			if strings.TrimSpace(author.Name) != "" {
				authors = append(authors, author.Name)
			}
		}
		pdfURL := ""
		if item.OpenAccessPDF != nil {
			pdfURL = item.OpenAccessPDF.URL
		}
		papers = append(papers, Paper{
			Provider:      t.Name(),
			ExternalID:    item.PaperID,
			Title:         item.Title,
			Abstract:      item.Abstract,
			Authors:       authors,
			Year:          item.Year,
			URL:           item.URL,
			PDFURL:        pdfURL,
			CitationCount: item.CitationCount,
		})
	}
	return papers, nil
}
