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

type OpenAlexTool struct {
	client *http.Client
}

func NewOpenAlexTool() *OpenAlexTool {
	return &OpenAlexTool{client: &http.Client{Timeout: 20 * time.Second}}
}

func (t *OpenAlexTool) Name() string {
	return "openalex"
}

func (t *OpenAlexTool) Search(ctx context.Context, query string, limit int) ([]Paper, error) {
	if limit <= 0 {
		limit = 10
	}

	endpoint, err := url.Parse("https://api.openalex.org/works")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("search", query)
	values.Set("per-page", fmt.Sprintf("%d", limit))
	values.Set("filter", "is_oa:true")
	values.Set("sort", "relevance_score:desc")
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
		return nil, fmt.Errorf("openalex returned status %s", resp.Status)
	}

	var body struct {
		Results []struct {
			ID                    string           `json:"id"`
			DOI                   string           `json:"doi"`
			DisplayName           string           `json:"display_name"`
			PublicationYear       int              `json:"publication_year"`
			CitedByCount          int              `json:"cited_by_count"`
			AbstractInvertedIndex map[string][]int `json:"abstract_inverted_index"`
			Authorships           []struct {
				Author struct {
					DisplayName string `json:"display_name"`
				} `json:"author"`
			} `json:"authorships"`
			PrimaryLocation *struct {
				LandingPageURL string `json:"landing_page_url"`
				PDFURL         string `json:"pdf_url"`
			} `json:"primary_location"`
			OpenAccess *struct {
				OAURL string `json:"oa_url"`
			} `json:"open_access"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	papers := make([]Paper, 0, len(body.Results))
	for _, item := range body.Results {
		title := normalizeText(item.DisplayName)
		id := strings.TrimSpace(firstNonEmpty([]string{item.DOI, item.ID}))
		if title == "" || id == "" {
			continue
		}

		authors := make([]string, 0, len(item.Authorships))
		for _, authorship := range item.Authorships {
			name := normalizeText(authorship.Author.DisplayName)
			if name != "" {
				authors = append(authors, name)
			}
		}

		urlValue := ""
		pdfURL := ""
		if item.PrimaryLocation != nil {
			urlValue = item.PrimaryLocation.LandingPageURL
			pdfURL = item.PrimaryLocation.PDFURL
		}
		if urlValue == "" {
			urlValue = item.ID
		}

		papers = append(papers, Paper{
			Provider:      t.Name(),
			ExternalID:    id,
			Title:         title,
			Abstract:      reconstructOpenAlexAbstract(item.AbstractInvertedIndex),
			Authors:       authors,
			Year:          item.PublicationYear,
			URL:           urlValue,
			PDFURL:        pdfURL,
			CitationCount: item.CitedByCount,
		})
	}
	return papers, nil
}

func reconstructOpenAlexAbstract(index map[string][]int) string {
	if len(index) == 0 {
		return ""
	}

	type positionedWord struct {
		word     string
		position int
	}
	words := make([]positionedWord, 0)
	for word, positions := range index {
		for _, position := range positions {
			words = append(words, positionedWord{word: word, position: position})
		}
	}
	sort.Slice(words, func(i, j int) bool {
		return words[i].position < words[j].position
	})

	ordered := make([]string, 0, len(words))
	for _, item := range words {
		ordered = append(ordered, item.word)
	}
	return normalizeText(strings.Join(ordered, " "))
}
