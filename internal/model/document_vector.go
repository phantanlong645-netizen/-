package model

// DocumentVector 记录文档分块文本。
// 注意：这里没有直接存向量，向量主要写入 Elasticsearch。
// 数据库里保存分块文本，是为了可追溯、重建索引、排查问题。
type DocumentVector struct {
	VectorID     uint   `gorm:"primaryKey;autoIncrement;column:vector_id"`
	FileMD5      string `gorm:"type:varchar(32);not null;index;column:file_md5"`
	ChunkID      int    `gorm:"not null;column:chunk_id"`
	TextContent  string `gorm:"type:text;column:text_content"`
	ModelVersion string `gorm:"type:varchar(50);column:model_version"`
	UserID       uint   `gorm:"not null;column:user_id"`
	OrgTag       string `gorm:"type:varchar(50);column:org_tag"`
	IsPublic     bool   `gorm:"not null;default:false;column:is_public"`
}

func (DocumentVector) TableName() string {
	return "document_vectors"
}
