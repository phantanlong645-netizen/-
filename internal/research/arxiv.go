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

type ArxivTool struct {
	client *http.Client
}

func NewArxivTool() *ArxivTool {
	return &ArxivTool{client: &http.Client{Timeout: 20 * time.Second}}
}

func (t *ArxivTool) Name() string {
	return "arxiv"
}

func (t *ArxivTool) Search(ctx context.Context, query string, limit int) ([]Paper, error) {
	if limit <= 0 {
		limit = 10
	}

	endpoint, err := url.Parse("https://export.arxiv.org/api/query")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("search_query", buildArxivSearchQuery(query))
	values.Set("start", "0")
	values.Set("max_results", fmt.Sprintf("%d", limit))
	values.Set("sortBy", "relevance")
	values.Set("sortOrder", "descending")
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "RAG-repository/1.0")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("arxiv returned status %s", resp.Status)
	}

	var feed arxivFeed
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, err
	}

	papers := make([]Paper, 0, len(feed.Entries))
	for _, entry := range feed.Entries {
		title := normalizeText(entry.Title)
		if title == "" {
			continue
		}
		id := strings.TrimSpace(entry.ID)
		pdfURL := ""
		for _, link := range entry.Links {
			if link.Title == "pdf" || link.Type == "application/pdf" {
				pdfURL = link.Href
				break
			}
		}
		authors := make([]string, 0, len(entry.Authors))
		for _, author := range entry.Authors {
			if strings.TrimSpace(author.Name) != "" {
				authors = append(authors, strings.TrimSpace(author.Name))
			}
		}
		papers = append(papers, Paper{
			Provider:   t.Name(),
			ExternalID: id,
			Title:      title,
			Abstract:   normalizeText(entry.Summary),
			Authors:    authors,
			Year:       parseYear(entry.Published),
			URL:        id,
			PDFURL:     pdfURL,
		})
	}
	return papers, nil
}

type arxivFeed struct {
	Entries []arxivEntry `xml:"entry"`
}

type arxivEntry struct {
	ID        string        `xml:"id"`
	Title     string        `xml:"title"`
	Summary   string        `xml:"summary"`
	Published string        `xml:"published"`
	Authors   []arxivAuthor `xml:"author"`
	Links     []arxivLink   `xml:"link"`
}

type arxivAuthor struct {
	Name string `xml:"name"`
}

type arxivLink struct {
	Href  string `xml:"href,attr"`
	Type  string `xml:"type,attr"`
	Title string `xml:"title,attr"`
}

func normalizeText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func parseYear(value string) int {
	if len(value) < 4 {
		return 0
	}
	year, _ := strconv.Atoi(value[:4])
	return year
}

func buildArxivSearchQuery(query string) string {
	replacer := strings.NewReplacer("(", " ", ")", " ", "[", " ", "]", " ", "{", " ", "}", " ", "*", " ")
	terms := strings.Fields(replacer.Replace(query))
	if len(terms) == 0 {
		return "all:" + query
	}

	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.Trim(term, `"'.,;:!?`)
		if term == "" || isArxivBooleanOperator(term) {
			continue
		}
		parts = append(parts, "all:"+term)
		if len(parts) >= 8 {
			break
		}
	}
	if len(parts) == 0 {
		return "all:" + query
	}
	return strings.Join(parts, " AND ")
}

func isArxivBooleanOperator(term string) bool {
	switch strings.ToUpper(term) {
	case "AND", "OR", "NOT":
		return true
	default:
		return false
	}
}
