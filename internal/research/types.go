package research

import "context"

type Paper struct {
	Provider      string
	ExternalID    string
	Title         string
	Abstract      string
	Authors       []string
	Year          int
	URL           string
	PDFURL        string
	CitationCount int
}

type SearchTool interface {
	Name() string
	Search(ctx context.Context, query string, limit int) ([]Paper, error)
}
