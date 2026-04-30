package repository

import (
	// context 用于控制 Redis 操作的生命周期。
	"context"

	// strconv 用来把 userID 转成字符串，拼 Redis key。
	"strconv"

	// 引入项目内部的数据库模型。
	"RAG-repository/internal/model"

	// Redis v9 客户端，你当前项目使用的是 v9。
	"github.com/redis/go-redis/v9"

	// GORM 数据库客户端。
	"gorm.io/gorm"
)

// UploadRepository 定义上传模块需要的数据访问能力。
// 它把 MySQL 和 Redis 的操作都封装起来，service 层不用关心底层存储细节。
type UploadRepository interface {
	// 创建文件上传主记录，写 MySQL 的 file_upload 表。
	CreateFileUploadRecord(record *model.FileUpload) error

	// 根据文件 MD5 和用户 ID 查询上传记录。
	GetFileUploadRecord(fileMD5 string, userID uint) (*model.FileUpload, error)

	// 更新文件上传状态，例如 uploading/completed/failed。
	UpdateFileUploadStatus(recordID uint, status int) error

	// 查询某个用户上传过的所有文件。
	FindFilesByUserID(userID uint) ([]model.FileUpload, error)

	// 查询某个用户有权限访问的文件。
	FindAccessibleFiles(userID uint, orgTags []string) ([]model.FileUpload, error)

	// 删除某个用户的文件上传记录。
	DeleteFileUploadRecord(fileMD5 string, userID uint) error

	// 更新整条文件上传记录。
	UpdateFileUploadRecord(record *model.FileUpload) error

	// 根据多个 MD5 批量查询文件记录，后面搜索结果补文件名会用。
	FindBatchByMD5s(md5s []string) ([]*model.FileUpload, error)

	// 创建分片记录，写 MySQL 的 chunk_info 表。
	CreateChunkInfoRecord(record *model.ChunkInfo) error

	// 查询某个文件的所有分片记录，用于合并文件。
	GetChunkInfoRecords(fileMD5 string) ([]model.ChunkInfo, error)

	// 查询某个分片是否已经上传过，读 Redis bitmap。
	IsChunkUploaded(ctx context.Context, fileMD5 string, userID uint, chunkIndex int) (bool, error)

	// 标记某个分片已经上传，写 Redis bitmap。
	MarkChunkUploaded(ctx context.Context, fileMD5 string, userID uint, chunkIndex int) error

	// 从 Redis bitmap 里读取所有已上传分片下标。
	GetUploadedChunksFromRedis(ctx context.Context, fileMD5 string, userID uint, totalChunks int) ([]int, error)

	// 删除 Redis 中的上传进度标记，通常在合并完成后清理。
	DeleteUploadMark(ctx context.Context, fileMD5 string, userID uint) error
}

// uploadRepository 是 UploadRepository 的具体实现。
// db 负责 MySQL，redisClient 负责 Redis。
type uploadRepository struct {
	db          *gorm.DB
	redisClient *redis.Client
}

// NewUploadRepository 创建上传仓储。
// main.go 里会传入 database.DB 和 database.RDB。
func NewUploadRepository(db *gorm.DB, redisClient *redis.Client) UploadRepository {
	return &uploadRepository{
		db:          db,
		redisClient: redisClient,
	}
}

// getRedisUploadKey 生成 Redis key。
// 同一个文件 MD5，不同用户可能都上传，所以 key 里必须带 userID。
func (r *uploadRepository) getRedisUploadKey(fileMD5 string, userID uint) string {
	return "upload:" + strconv.FormatUint(uint64(userID), 10) + ":" + fileMD5
}

// CreateFileUploadRecord 在 MySQL 中创建文件上传主记录。
func (r *uploadRepository) CreateFileUploadRecord(record *model.FileUpload) error {
	return r.db.Create(record).Error
}

// GetFileUploadRecord 根据 fileMD5 和 userID 查询文件上传记录。
// fileMD5 定位文件，userID 限定文件归属。
func (r *uploadRepository) GetFileUploadRecord(fileMD5 string, userID uint) (*model.FileUpload, error) {
	var record model.FileUpload

	err := r.db.
		Where("file_md5 = ? AND user_id = ?", fileMD5, userID).
		First(&record).
		Error
	if err != nil {
		return nil, err
	}

	return &record, nil
}

// FindBatchByMD5s 根据多个 MD5 批量查询文件记录。
// 搜索服务拿到 ES 命中结果后，需要根据 file_md5 批量查文件名。
func (r *uploadRepository) FindBatchByMD5s(md5s []string) ([]*model.FileUpload, error) {
	var records []*model.FileUpload

	if len(md5s) == 0 {
		return records, nil
	}

	err := r.db.
		Where("file_md5 IN ?", md5s).
		Find(&records).
		Error

	return records, err
}

// UpdateFileUploadStatus 更新文件状态。
// 例如 0 表示上传中，1 表示已完成，2 表示失败。
func (r *uploadRepository) UpdateFileUploadStatus(recordID uint, status int) error {
	return r.db.
		Model(&model.FileUpload{}).
		Where("id = ?", recordID).
		Update("status", status).
		Error
}

// GetChunkInfoRecords 查询某个文件的所有分片记录。
// 合并文件时要按 chunk_index 顺序取出分片。
func (r *uploadRepository) GetChunkInfoRecords(fileMD5 string) ([]model.ChunkInfo, error) {
	var chunks []model.ChunkInfo

	err := r.db.
		Where("file_md5 = ?", fileMD5).
		Order("chunk_index asc").
		Find(&chunks).
		Error

	return chunks, err
}

// FindFilesByUserID 查询用户自己上传的文件。
func (r *uploadRepository) FindFilesByUserID(userID uint) ([]model.FileUpload, error) {
	var files []model.FileUpload

	err := r.db.
		Where("user_id = ?", userID).
		Find(&files).
		Error

	return files, err
}

// FindAccessibleFiles 查询用户能访问的文件。
// 条件是：文件已完成，并且满足自己上传、公开文件、组织内公开文件之一。
func (r *uploadRepository) FindAccessibleFiles(userID uint, orgTags []string) ([]model.FileUpload, error) {
	var files []model.FileUpload

	err := r.db.
		Where("status = ?", 1).
		Where(
			r.db.Where("user_id = ?", userID).
				Or("is_public = ?", true).
				Or("org_tag IN ? AND is_public = ?", orgTags, true),
		).
		Find(&files).
		Error

	return files, err
}

// DeleteFileUploadRecord 删除某个用户的文件上传记录。
func (r *uploadRepository) DeleteFileUploadRecord(fileMD5 string, userID uint) error {
	return r.db.
		Where("file_md5 = ? AND user_id = ?", fileMD5, userID).
		Delete(&model.FileUpload{}).
		Error
}

// UpdateFileUploadRecord 保存整条文件上传记录。
func (r *uploadRepository) UpdateFileUploadRecord(record *model.FileUpload) error {
	return r.db.Save(record).Error
}

// CreateChunkInfoRecord 创建分片记录。
// 注意：分片文件本体在 MinIO，这里只记录分片元数据。
func (r *uploadRepository) CreateChunkInfoRecord(record *model.ChunkInfo) error {
	return r.db.Create(record).Error
}

// IsChunkUploaded 判断某个分片是否已经上传过。
// Redis bitmap 中，chunkIndex 对应一个 bit，1 表示已上传。
func (r *uploadRepository) IsChunkUploaded(ctx context.Context, fileMD5 string, userID uint, chunkIndex int) (bool, error) {
	key := r.getRedisUploadKey(fileMD5, userID)

	val, err := r.redisClient.
		GetBit(ctx, key, int64(chunkIndex)).
		Result()
	if err != nil {
		return false, err
	}

	return val == 1, nil
}

// MarkChunkUploaded 把某个分片标记为已上传。
// SetBit(key, chunkIndex, 1) 表示把第 chunkIndex 位设置为 1。
func (r *uploadRepository) MarkChunkUploaded(ctx context.Context, fileMD5 string, userID uint, chunkIndex int) error {
	key := r.getRedisUploadKey(fileMD5, userID)

	return r.redisClient.
		SetBit(ctx, key, int64(chunkIndex), 1).
		Err()
}

// GetUploadedChunksFromRedis 从 Redis bitmap 还原已上传的分片下标列表。
// 前端查询上传进度时，可以拿到已经上传过哪些分片。
func (r *uploadRepository) GetUploadedChunksFromRedis(ctx context.Context, fileMD5 string, userID uint, totalChunks int) ([]int, error) {
	if totalChunks == 0 {
		return []int{}, nil
	}

	key := r.getRedisUploadKey(fileMD5, userID)

	bitmap, err := r.redisClient.
		Get(ctx, key).
		Bytes()
	if err != nil {
		if err == redis.Nil {
			return []int{}, nil
		}

		return nil, err
	}

	uploaded := make([]int, 0)

	for i := 0; i < totalChunks; i++ {
		byteIndex := i / 8
		bitIndex := i % 8

		if byteIndex < len(bitmap) && (bitmap[byteIndex]>>(7-bitIndex))&1 == 1 {
			uploaded = append(uploaded, i)
		}
	}

	return uploaded, nil
}

// DeleteUploadMark 删除 Redis 里的上传进度。
// 文件合并完成后，分片进度就不需要继续保留了。
func (r *uploadRepository) DeleteUploadMark(ctx context.Context, fileMD5 string, userID uint) error {
	key := r.getRedisUploadKey(fileMD5, userID)

	return r.redisClient.
		Del(ctx, key).
		Err()
}
