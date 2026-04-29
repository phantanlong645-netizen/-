package model

// SearchResponseDTO 是搜索接口返回给前端的数据结构。
// 后面 search service 会把 ES 命中的文档转换成这个结构。
type SearchResponseDTO struct {
	FileMD5     string  `json:"fileMd5"`
	FileName    string  `json:"fileName"`
	ChunkID     int     `json:"chunkId"`
	TextContent string  `json:"textContent"`
	Score       float64 `json:"score"`
	UserID      string  `json:"userId"`
	OrgTag      string  `json:"orgTag"`
	IsPublic    bool    `json:"isPublic"`
}

// EsDocument 表示真正写入 Elasticsearch 的文档结构。
// 一个原始文件会被拆成多个 chunk，每个 chunk 对应一条 EsDocument。
type EsDocument struct {
	VectorID     string    `json:"vector_id"`
	FileMD5      string    `json:"file_md5"`
	ChunkID      int       `json:"chunk_id"`
	TextContent  string    `json:"text_content"`
	Vector       []float32 `json:"vector"`
	ModelVersion string    `json:"model_version"`
	UserID       uint      `json:"user_id"`
	OrgTag       string    `json:"org_tag"`
	IsPublic     bool      `json:"is_public"`
}
