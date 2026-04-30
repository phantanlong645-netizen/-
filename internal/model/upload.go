package model

import "time"

// FileUpload 记录一个文件上传任务的元数据。
// 它不是文件本体，文件本体在 MinIO，这里只存文件名、MD5、大小、状态等信息。
type FileUpload struct {
	ID        uint       `gorm:"primaryKey;autoIncrement" json:"id"`
	FileMD5   string     `gorm:"type:varchar(32);not null" json:"fileMd5"`
	FileName  string     `gorm:"type:varchar(255);not null" json:"fileName"`
	TotalSize int64      `gorm:"not null" json:"totalSize"`
	Status    int        `gorm:"type:tinyint;not null;default:0" json:"status"`
	UserID    uint       `gorm:"not null" json:"userId"`
	OrgTag    string     `gorm:"type:varchar(50)" json:"orgTag"`
	IsPublic  bool       `gorm:"not null;default:false" json:"isPublic"`
	CreatedAt time.Time  `gorm:"autoCreateTime" json:"createdAt"`
	MergedAt  *time.Time `gorm:"default:null" json:"mergedAt"`
}

func (FileUpload) TableName() string {
	return "file_upload"
}

// ChunkInfo 记录文件分片信息。
// 大文件上传时，前端会分片上传，每个分片都会有一条记录。
type ChunkInfo struct {
	ID          uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	FileMD5     string `gorm:"type:varchar(32);not null" json:"fileMd5"`
	ChunkIndex  int    `gorm:"not null" json:"chunkIndex"`
	ChunkMD5    string `gorm:"type:varchar(32);not null" json:"chunkMd5"`
	StoragePath string `gorm:"type:varchar(255);not null" json:"storagePath"`
}

func (ChunkInfo) TableName() string {
	return "chunk_info"
}
