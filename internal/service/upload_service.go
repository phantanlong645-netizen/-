// Package service 放业务层代码。
package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"mime/multipart"
	"strings"

	"RAG-repository/internal/config"
	"RAG-repository/internal/model"
	"RAG-repository/internal/repository"
	"RAG-repository/pkg/kafka"
	"RAG-repository/pkg/log"
	"RAG-repository/pkg/storage"
	"RAG-repository/pkg/tasks"

	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
)

const (
	// DefaultChunkSize 表示默认分片大小，5MB 一个分片。
	DefaultChunkSize = 5 * 1024 * 1024
)

// UploadService 定义上传模块对外暴露的业务能力。
type UploadService interface {
	// CheckFile 检查文件是否已经上传过，支持秒传和断点续传查询。
	CheckFile(ctx context.Context, fileMD5 string, userID uint) (bool, []int, error)
	// UploadChunk 上传一个文件分片，并记录这个分片已经上传完成。
	UploadChunk(ctx context.Context, fileMD5, fileName string, totalSize int64, chunkIndex int, file multipart.File, userID uint, orgTag string, isPublic bool) (uploadedChunks []int, totalChunks int, err error)
	// MergeChunks 把所有分片合并成完整文件。
	MergeChunks(ctx context.Context, fileMD5, fileName string, userID uint) (string, error)
	// GetUploadStatus 查询某个文件当前已经上传了哪些分片。
	GetUploadStatus(ctx context.Context, fileMD5 string, userID uint) (fileName string, fileType string, uploadedChunks []int, totalChunks int, err error)
	// GetSupportedFileTypes 返回系统支持上传和解析的文件类型。
	GetSupportedFileTypes() (map[string]interface{}, error)
	// FastUpload 单独做一次秒传检查。
	FastUpload(ctx context.Context, fileMD5 string, userID uint) (bool, error)
}

// uploadService 是 UploadService 的具体实现。
type uploadService struct {
	// uploadRepo 负责文件上传记录、分片记录、Redis bitmap 的读写。
	uploadRepo repository.UploadRepository
	// userRepo 用来在没有传 orgTag 时查询用户主组织。
	userRepo repository.UserRepository
	// minioCfg 保存 MinIO bucket 等配置，业务层上传和合并文件时要用。
	minioCfg config.MinIOConfig
}

// NewUploadService 创建上传业务对象，并把依赖注入进来。
func NewUploadService(uploadRepo repository.UploadRepository, userRepo repository.UserRepository, minioCfg config.MinIOConfig) UploadService {
	// 返回接口类型，外部只依赖 UploadService，不直接依赖 uploadService 结构体。
	return &uploadService{
		// 保存上传仓储，后面所有数据库和 Redis 操作都通过它完成。
		uploadRepo: uploadRepo,
		// 保存用户仓储，用来补全用户组织信息。
		userRepo: userRepo,
		// 保存 MinIO 配置，用来确定文件存到哪个 bucket。
		minioCfg: minioCfg,
	}
}

// CheckFile 检查文件是否能秒传，或者返回已经上传过的分片列表。
func (s *uploadService) CheckFile(ctx context.Context, fileMD5 string, userID uint) (bool, []int, error) {
	// 打日志，方便排查用户上传同一个 MD5 文件时的流程。
	log.Infof("[CheckFile] 开始秒传检查，文件MD5: %s, 用户ID: %d", fileMD5, userID)

	// 先用文件 MD5 和用户 ID 查询上传记录。
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, userID)
	// 如果查询出错，需要区分“没记录”和“真的异常”。
	if err != nil {
		// 没有记录说明这个用户没有上传过该文件，不能秒传，需要普通上传。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 这里返回 false 表示不能秒传，nil 表示没有任何已上传分片。
			log.Infof("[CheckFile] 文件记录不存在，需要进行普通上传。文件MD5: %s", fileMD5)
			return false, nil, nil
		}
		// 其他错误一般是数据库问题，直接往上返回。
		log.Errorf("[CheckFile] 秒传检查失败：查询文件记录时出错，error: %v", err)
		return false, nil, err
	}

	// status=1 表示完整文件已经合并完成，可以直接秒传。
	if record.Status == 1 {
		// 秒传成功时不需要返回分片列表，因为前端不用继续传分片。
		log.Infof("[CheckFile] 文件已存在且状态为已完成，秒传成功。文件MD5: %s", fileMD5)
		return true, nil, nil
	}

	// 走到这里说明上传记录存在，但文件还没有合并完成。
	totalChunks := s.calculateTotalChunks(record.TotalSize)
	// 从 Redis bitmap 中读取已经上传过的分片下标。
	uploadedIndexes, err := s.uploadRepo.GetUploadedChunksFromRedis(ctx, fileMD5, userID, totalChunks)
	// Redis 查询失败时，不能准确告诉前端续传进度，所以返回错误。
	if err != nil {
		log.Errorf("[CheckFile] 秒传检查失败：从Redis获取已上传分片列表时出错，error: %v", err)
		return false, nil, err
	}
	// 返回 false 表示不能秒传，但可以根据 uploadedIndexes 做断点续传。
	log.Infof("[CheckFile] 文件记录已存在但未完成，返回已上传的分片列表。文件MD5: %s, 已上传分片数: %d", fileMD5, len(uploadedIndexes))
	return false, uploadedIndexes, nil
}

// UploadChunk 处理单个分片上传。
func (s *uploadService) UploadChunk(ctx context.Context, fileMD5, fileName string, totalSize int64, chunkIndex int, file multipart.File, userID uint, orgTag string, isPublic bool) ([]int, int, error) {
	// 打日志记录当前上传的是哪个文件、哪个分片、哪个用户。
	log.Infof("[UploadChunk] 开始上传分片，文件MD5: %s, 分片序号: %d, 用户ID: %d", fileMD5, chunkIndex, userID)

	// 只在第 0 个分片校验文件类型，避免每个分片都重复校验。
	if chunkIndex == 0 {
		// 获取系统支持的文件类型配置。
		supportedTypes, _ := s.GetSupportedFileTypes()
		// 从 map 中取出支持的扩展名列表。
		extensions, ok := supportedTypes["supportedExtensions"].([]string)
		// 如果类型断言失败，说明 GetSupportedFileTypes 返回结构不符合预期。
		if !ok {
			return nil, 0, errors.New("invalid supported types configuration")
		}
		// isValid 用来标记当前文件后缀是否合法。
		isValid := false
		// 遍历所有允许的扩展名。
		for _, ext := range extensions {
			// 转小写后判断文件名后缀，避免 PDF/pdf 大小写问题。
			if strings.HasSuffix(strings.ToLower(fileName), ext) {
				// 找到匹配后缀，说明文件类型允许上传。
				isValid = true
				// 已经匹配成功，不需要继续遍历。
				break
			}
		}
		// 如果后缀不在白名单里，直接拒绝上传。
		if !isValid {
			return nil, 0, fmt.Errorf("unsupported file type for %s", fileName)
		}
	}

	// 查询这个文件是否已经有上传主记录。
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, userID)
	// 如果主记录不存在，说明这是该文件第一次上传分片，需要先创建文件上传记录。
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 记录日志，说明下面会创建 file_uploads 表记录。
		log.Infof("[UploadChunk] 文件上传记录不存在，为文件MD5: %s 创建新记录", fileMD5)
		// 如果前端没有传组织标签，就默认使用用户的主组织。
		if orgTag == "" {
			// 根据 userID 查询用户信息。
			user, userErr := s.userRepo.FindByID(userID)
			// 查询用户失败就不能补全 orgTag，直接返回错误。
			if userErr != nil {
				return nil, 0, userErr
			}
			// 使用用户主组织作为文件组织标签。
			orgTag = user.PrimaryOrg
		}

		// 构造文件上传主记录。
		newRecord := &model.FileUpload{
			// FileMD5 用来唯一标识文件内容，后续秒传和合并都依赖它。
			FileMD5: fileMD5,
			// FileName 保存用户上传时的原始文件名。
			FileName: fileName,
			// TotalSize 保存完整文件大小，用来计算总分片数。
			TotalSize: totalSize,
			// Status=0 表示上传中，Status=1 表示合并完成。
			Status: 0,
			// UserID 表示这个上传记录属于哪个用户。
			UserID: userID,
			// OrgTag 表示该文件归属哪个组织权限范围。
			OrgTag: orgTag,
			// IsPublic 表示该文件是否公开给其他可见范围。
			IsPublic: isPublic,
		}
		// 把文件上传主记录写入数据库。
		if err := s.uploadRepo.CreateFileUploadRecord(newRecord); err != nil {
			// 数据库写入失败就不能继续上传。
			log.Errorf("[UploadChunk] 创建文件上传记录失败，error: %v", err)
			return nil, 0, err
		}
		// 后续逻辑统一使用 record，所以把新建记录赋值给 record。
		record = newRecord
	} else if err != nil {
		// 如果不是“记录不存在”，就是数据库查询异常。
		log.Errorf("[UploadChunk] 查询文件上传记录失败，error: %v", err)
		return nil, 0, err
	}

	// 用 Redis bitmap 检查当前分片是否已经上传过。
	isUploaded, err := s.uploadRepo.IsChunkUploaded(ctx, fileMD5, userID, chunkIndex)
	// Redis 查询失败会导致无法判断重复上传，所以直接返回错误。
	if err != nil {
		log.Errorf("[UploadChunk] 从Redis检查分片上传状态失败，error: %v", err)
		return nil, 0, fmt.Errorf("failed to check chunk status from redis: %w", err)
	}
	// 如果该分片已经上传过，就不再重复写 MinIO。
	if isUploaded {
		// 记录跳过上传的原因。
		log.Infof("[UploadChunk] 分片 %d 已上传过，跳过本次上传。文件MD5: %s", chunkIndex, fileMD5)
		// 重新计算总分片数，用于返回给前端。
		totalChunks := s.calculateTotalChunks(record.TotalSize)
		// 读取当前所有已经上传过的分片下标。
		uploadedIndexes, err := s.uploadRepo.GetUploadedChunksFromRedis(ctx, fileMD5, userID, totalChunks)
		// 读取失败就返回错误。
		if err != nil {
			return nil, 0, err
		}
		// 返回当前进度，让前端知道哪些分片不用再传。
		return uploadedIndexes, totalChunks, nil
	}

	// 生成分片在 MinIO 里的对象名，所有分片都放在 chunks/{fileMD5}/ 目录下。
	objectName := fmt.Sprintf("chunks/%s/%d", fileMD5, chunkIndex)
	// 把当前分片内容上传到 MinIO。
	_, err = storage.MinioClient.PutObject(ctx, s.minioCfg.BucketName, objectName, file, -1, minio.PutObjectOptions{})
	// MinIO 上传失败时，不能标记分片完成。
	if err != nil {
		log.Errorf("[UploadChunk] 上传分片到MinIO失败，objectName: %s, error: %v", objectName, err)
		return nil, 0, err
	}

	// 构造分片记录，用数据库保存分片元信息。
	chunkRecord := &model.ChunkInfo{
		// FileMD5 表示这个分片属于哪个文件。
		FileMD5: fileMD5,
		// ChunkIndex 表示这是第几个分片。
		ChunkIndex: chunkIndex,
		// ChunkMD5 当前 Go 版本暂时不计算单个分片 MD5。
		ChunkMD5: "",
		// StoragePath 保存分片在 MinIO 里的对象路径。
		StoragePath: objectName,
	}
	// 把分片元信息写入数据库。
	if err := s.uploadRepo.CreateChunkInfoRecord(chunkRecord); err != nil {
		// 注意：这里如果失败，MinIO 里已经有分片对象，生产环境最好补偿清理。
		log.Errorf("[UploadChunk] 在数据库中创建分片记录失败，error: %v", err)
		return nil, 0, err
	}

	// 数据库记录成功后，在 Redis bitmap 里把该分片标记为已上传。
	if err := s.uploadRepo.MarkChunkUploaded(ctx, fileMD5, userID, chunkIndex); err != nil {
		// Redis 标记失败会影响断点续传和合并完整性判断，所以这里直接返回错误。
		log.Errorf("[UploadChunk] 严重错误：在Redis中标记分片已上传失败，error: %v", err)
		return nil, 0, err
	}

	// 根据完整文件大小计算总分片数。
	totalChunks := s.calculateTotalChunks(record.TotalSize)
	// 从 Redis bitmap 里读取最新上传进度。
	uploadedIndexes, err := s.uploadRepo.GetUploadedChunksFromRedis(ctx, fileMD5, userID, totalChunks)
	// 如果读取失败，虽然分片已上传，但前端拿不到最新进度，所以返回错误。
	if err != nil {
		log.Errorf("[UploadChunk] 上传成功后从Redis获取最新分片列表失败，error: %v", err)
		return nil, 0, err
	}

	// 正常返回当前已上传分片列表和总分片数。
	log.Infof("[UploadChunk] 分片上传成功。文件MD5: %s, 分片序号: %d, 总进度: %d/%d", fileMD5, chunkIndex, len(uploadedIndexes), totalChunks)
	return uploadedIndexes, totalChunks, nil
}

// MergeChunks 合并一个文件的所有分片。
func (s *uploadService) MergeChunks(ctx context.Context, fileMD5, fileName string, userID uint) (string, error) {
	// 打日志记录合并请求。
	log.Infof("[MergeChunks] 开始合并文件分片，文件MD5: %s, 用户ID: %d", fileMD5, userID)
	// 先查文件上传主记录，拿到文件大小、组织权限、公开状态等信息。
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, userID)
	// 主记录不存在或数据库异常时，不能合并。
	if err != nil {
		log.Errorf("[MergeChunks] 合并分片失败：获取文件记录时出错，error: %v", err)
		return "", err
	}

	// 根据文件总大小计算应该有多少个分片。
	totalChunks := s.calculateTotalChunks(record.TotalSize)
	// 从 Redis bitmap 读取已经上传完成的分片下标。
	uploadedIndexes, err := s.uploadRepo.GetUploadedChunksFromRedis(ctx, fileMD5, userID, totalChunks)
	// Redis 读取失败时无法判断分片完整性，所以拒绝合并。
	if err != nil {
		log.Errorf("[MergeChunks] 合并分片失败：从Redis检查分片完整性时出错，error: %v", err)
		return "", fmt.Errorf("failed to get uploaded chunks from redis: %w", err)
	}
	// 如果已上传分片数量小于总分片数，说明文件还没传完整。
	if len(uploadedIndexes) < totalChunks {
		// 这里必须拒绝，否则 MinIO 合并时会缺对象。
		log.Warnf("[MergeChunks] 拒绝合并请求：分片未完全上传。文件MD5: %s, 期望分片数: %d, 实际分片数: %d", fileMD5, totalChunks, len(uploadedIndexes))
		return "", fmt.Errorf("分片未全部上传，无法合并 (期望: %d, 实际: %d)", totalChunks, len(uploadedIndexes))
	}

	// 生成完整文件在 MinIO 里的对象名。
	destObjectName := fmt.Sprintf("merged/%s", fileName)

	// 如果只有一个分片，不需要真正 compose，直接 copy 成最终文件。
	if totalChunks == 1 {
		// 构造源对象，也就是第 0 个分片。
		src := minio.CopySrcOptions{
			// Bucket 是分片所在 bucket。
			Bucket: s.minioCfg.BucketName,
			// Object 是分片对象路径。
			Object: fmt.Sprintf("chunks/%s/0", fileMD5),
		}
		// 构造目标对象，也就是合并后的文件。
		dst := minio.CopyDestOptions{
			// Bucket 是最终文件所在 bucket。
			Bucket: s.minioCfg.BucketName,
			// Object 是最终文件对象路径。
			Object: destObjectName,
		}
		// 调 MinIO CopyObject，把单分片复制成 merged 文件。
		_, err = storage.MinioClient.CopyObject(context.Background(), dst, src)
		// CopyObject 失败就返回错误。
		if err != nil {
			log.Errorf("[MergeChunks] 单分片文件复制失败，error: %v", err)
			return "", fmt.Errorf("failed to copy single chunk object: %w", err)
		}
		// 单分片合并完成。
		log.Infof("[MergeChunks] 单分片文件复制成功。")
	} else {
		// 多分片文件使用 MinIO ComposeObject 在服务端合并，避免下载到本地再拼接。
		var srcs []minio.CopySrcOptions
		// 按分片下标从 0 到 totalChunks-1 生成源对象列表。
		for i := 0; i < totalChunks; i++ {
			// 每个源对象都对应 chunks/{fileMD5}/{i}。
			srcs = append(srcs, minio.CopySrcOptions{
				// Bucket 是分片所在 bucket。
				Bucket: s.minioCfg.BucketName,
				// Object 是当前分片路径。
				Object: fmt.Sprintf("chunks/%s/%d", fileMD5, i),
			})
		}

		// 构造合并后的目标对象。
		dst := minio.CopyDestOptions{
			// Bucket 是最终文件所在 bucket。
			Bucket: s.minioCfg.BucketName,
			// Object 是合并后的文件路径。
			Object: destObjectName,
		}
		// 调 MinIO ComposeObject 合并所有分片。
		_, err = storage.MinioClient.ComposeObject(context.Background(), dst, srcs...)
		// 合并失败通常是某个分片对象不存在或 MinIO 异常。
		if err != nil {
			log.Errorf("[MergeChunks] 多分片文件合并失败，error: %v", err)
			return "", err
		}
		// 多分片合并完成。
		log.Infof("[MergeChunks] 多分片文件合并成功。")
	}

	// MinIO 合并成功后，把数据库状态更新为 1，表示上传完成。
	if err := s.uploadRepo.UpdateFileUploadStatus(record.ID, 1); err != nil {
		// 数据库状态更新失败会导致后续秒传查不到完成状态。
		log.Errorf("[MergeChunks] 更新数据库文件状态为“已完成”失败，error: %v", err)
		return "", err
	}
	// 记录状态更新成功。
	log.Infof("[MergeChunks] 数据库文件状态已更新为“已完成”。文件ID: %d", record.ID)

	// 给合并后的对象生成一个临时访问 URL。
	objectURL, _ := storage.GetPresignedURL(s.minioCfg.BucketName, destObjectName, 60*60)
	// 构造文件处理任务，后续消费者会解析文件、切片、向量化、入库。
	task := tasks.FileProcessingTask{
		// FileMD5 让异步任务能定位文件。
		FileMD5: fileMD5,
		// ObjectUrl 是消费者下载文件的临时地址。
		ObjectUrl: objectURL,
		// FileName 是原始文件名。
		FileName: fileName,
		// UserID 表示文件属于哪个用户。
		UserID: userID,
		// OrgTag 控制后续知识库检索权限。
		OrgTag: record.OrgTag,
		// IsPublic 控制文件是否公开。
		IsPublic: record.IsPublic,
	}
	// 把任务发到 Kafka，触发后面的文件解析流水线。
	if err := kafka.ProduceFileTask(task); err != nil {
		// Kafka 失败只记录日志，不阻断上传返回，因为文件已经合并成功。
		log.Errorf("[MergeChunks] 发送文件处理任务到Kafka失败，error: %v", err)
	} else {
		// Kafka 成功表示异步处理流程已经被触发。
		log.Infof("[MergeChunks] 文件处理任务已成功发送到Kafka。")
	}

	// 启动后台 goroutine 清理 Redis 上传标记和 MinIO 分片对象。
	go func() {
		// 后台任务使用独立 context，避免请求结束导致清理被取消。
		bgCtx := context.Background()
		// 记录清理开始。
		log.Infof("[MergeChunks] 启动后台清理任务。文件MD5: %s", fileMD5)
		// 删除 Redis bitmap 上传进度标记。
		if err := s.uploadRepo.DeleteUploadMark(bgCtx, fileMD5, userID); err != nil {
			// 清理失败不影响主流程，只记录警告。
			log.Warnf("[MergeChunks] 后台清理任务：删除Redis上传标记失败，fileMD5: %s, error: %v", fileMD5, err)
		}

		// 创建对象通道，MinIO RemoveObjects 通过 channel 批量接收待删除对象。
		objectsCh := make(chan minio.ObjectInfo)
		// 启动一个 goroutine 往通道里写入所有分片对象。
		go func() {
			// 写完所有对象后关闭通道，通知 RemoveObjects 没有更多对象。
			defer close(objectsCh)
			// 遍历所有分片下标。
			for i := 0; i < totalChunks; i++ {
				// 把分片对象路径写入删除通道。
				objectsCh <- minio.ObjectInfo{Key: fmt.Sprintf("chunks/%s/%d", fileMD5, i)}
			}
		}()
		// RemoveObjects 是批量删除接口，这里 fire-and-forget，不阻塞接口返回。
		for range storage.MinioClient.RemoveObjects(bgCtx, s.minioCfg.BucketName, objectsCh, minio.RemoveObjectsOptions{}) {
			// 当前实现忽略单个对象删除错误；生产环境可以在这里记录错误明细。
		}
		// 记录清理完成。
		log.Infof("[MergeChunks] 后台清理任务完成。文件MD5: %s", fileMD5)
	}()

	// 返回完整文件的临时访问 URL。
	return objectURL, nil
}

// GetUploadStatus 获取文件上传状态。
func (s *uploadService) GetUploadStatus(ctx context.Context, fileMD5 string, userID uint) (string, string, []int, int, error) {
	// 打日志，记录查询的是哪个文件。
	log.Infof("[GetUploadStatus] 开始获取文件上传状态。文件MD5: %s", fileMD5)
	// 查询文件上传主记录。
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, userID)
	// 查询失败时无法知道文件名、总大小和状态。
	if err != nil {
		log.Errorf("[GetUploadStatus] 获取文件上传状态失败：查询文件记录时出错，error: %v", err)
		return "", "", nil, 0, err
	}

	// 根据文件总大小计算总分片数。
	totalChunks := s.calculateTotalChunks(record.TotalSize)
	// 从 Redis bitmap 读取已上传分片列表。
	uploadedIndexes, err := s.uploadRepo.GetUploadedChunksFromRedis(ctx, fileMD5, userID, totalChunks)
	// Redis 失败时不能准确返回断点续传进度。
	if err != nil {
		log.Errorf("[GetUploadStatus] 获取文件上传状态失败：从Redis获取已上传分片列表时出错，error: %v", err)
		return "", "", nil, 0, err
	}

	// 根据文件名后缀推断文件类型描述。
	fileType := getFileType(record.FileName)
	// 正常返回文件名、文件类型、已上传分片、总分片数。
	log.Infof("[GetUploadStatus] 成功获取文件上传状态。文件MD5: %s", fileMD5)
	return record.FileName, fileType, uploadedIndexes, totalChunks, nil
}

// GetSupportedFileTypes 返回当前系统支持的文档类型。
func (s *uploadService) GetSupportedFileTypes() (map[string]interface{}, error) {
	// 打日志，记录文件类型查询。
	log.Info("[GetSupportedFileTypes] 开始获取系统支持的文件类型")
	// typeMapping 是扩展名到中文类型名称的映射。
	typeMapping := map[string]string{
		// .pdf 表示 PDF 文档。
		".pdf": "PDF文档",
		// .doc 表示老版本 Word 文档。
		".doc": "Word文档",
		// .docx 表示新版 Word 文档。
		".docx": "Word文档",
		// .xls 表示老版本 Excel 表格。
		".xls": "Excel表格",
		// .xlsx 表示新版 Excel 表格。
		".xlsx": "Excel表格",
		// .ppt 表示老版本 PowerPoint。
		".ppt": "PowerPoint演示文稿",
		// .pptx 表示新版 PowerPoint。
		".pptx": "PowerPoint演示文稿",
		// .txt 表示纯文本文件。
		".txt": "文本文件",
		// .md 表示 Markdown 文档。
		".md": "Markdown文档",
	}

	// supportedExtensions 保存所有支持的扩展名。
	supportedExtensions := make([]string, 0, len(typeMapping))
	// supportedTypes 保存所有支持的文件类型名称。
	supportedTypes := make([]string, 0, len(typeMapping))
	// uniqueTypes 用来去重，比如 .doc 和 .docx 都是 Word文档。
	uniqueTypes := make(map[string]struct{})

	// 遍历扩展名和类型映射。
	for ext, t := range typeMapping {
		// 把扩展名加入返回列表。
		supportedExtensions = append(supportedExtensions, ext)
		// 如果这个类型名称还没加入过，就加入 supportedTypes。
		if _, exists := uniqueTypes[t]; !exists {
			// 标记这个类型名称已经出现过。
			uniqueTypes[t] = struct{}{}
			// 把去重后的类型名称加入返回列表。
			supportedTypes = append(supportedTypes, t)
		}
	}

	// description 是给前端展示用的说明文字。
	description := "系统支持的文档类型文件，这些文件可以被解析并进行向量化处理"

	// data 是最终返回给 handler 的结构。
	data := map[string]interface{}{
		// supportedExtensions 返回扩展名白名单。
		"supportedExtensions": supportedExtensions,
		// supportedTypes 返回文件类型名称列表。
		"supportedTypes": supportedTypes,
		// description 返回说明文案。
		"description": description,
	}
	// 打日志表示获取成功。
	log.Info("[GetSupportedFileTypes] 成功获取系统支持的文件类型。")
	// 返回文件类型配置。
	return data, nil
}

// FastUpload 专门检查一个文件是否可以秒传。
func (s *uploadService) FastUpload(ctx context.Context, fileMD5 string, userID uint) (bool, error) {
	// 打日志记录秒传检查。
	log.Infof("[FastUpload] 开始秒传（快速上传）检查。文件MD5: %s", fileMD5)
	// 查询该用户是否有这个文件的上传记录。
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, userID)
	// 查询出错时需要区分记录不存在和数据库异常。
	if err != nil {
		// 记录不存在说明不能秒传。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 返回 false，err 为 nil，表示这是正常业务结果。
			log.Info("[FastUpload] 秒传检查：文件记录不存在，无法秒传。")
			return false, nil
		}
		// 数据库异常需要返回给调用方。
		log.Errorf("[FastUpload] 秒传检查失败：查询数据库时出错，error: %v", err)
		return false, err
	}
	// status=1 才表示完整文件已合并完成。
	log.Infof("[FastUpload] 秒传检查：文件记录已存在，状态为 %d。", record.Status)
	// 返回 true 表示可以秒传，false 表示文件还没完成。
	return record.Status == 1, nil
}

// calculateTotalChunks 根据文件大小计算总分片数。
func (s *uploadService) calculateTotalChunks(totalSize int64) int {
	// 如果文件大小是 0，就认为没有分片。
	if totalSize == 0 {
		return 0
	}
	// 用向上取整计算分片数，例如 6MB 文件按 5MB 分片会得到 2 个分片。
	return int(math.Ceil(float64(totalSize) / float64(DefaultChunkSize)))
}

// getFileType 根据文件名后缀推断文件类型描述。
func getFileType(fileName string) string {
	// 空文件名无法判断类型。
	if fileName == "" {
		return "未知类型"
	}
	// 用点号切分文件名，最后一段就是扩展名。
	parts := strings.Split(fileName, ".")
	// 如果没有点号，说明没有扩展名。
	if len(parts) < 2 {
		return "未知类型"
	}
	// 拼回带点的扩展名，并统一转小写。
	ext := "." + strings.ToLower(parts[len(parts)-1])

	// typeMapping 保存扩展名到中文文件类型的映射。
	typeMapping := map[string]string{
		// .pdf 表示 PDF 文档。
		".pdf": "PDF文档",
		// .doc 表示老版本 Word 文档。
		".doc": "Word文档",
		// .docx 表示新版 Word 文档。
		".docx": "Word文档",
		// .xls 表示老版本 Excel 表格。
		".xls": "Excel表格",
		// .xlsx 表示新版 Excel 表格。
		".xlsx": "Excel表格",
		// .ppt 表示老版本 PowerPoint。
		".ppt": "PowerPoint演示文稿",
		// .pptx 表示新版 PowerPoint。
		".pptx": "PowerPoint演示文稿",
		// .txt 表示纯文本文件。
		".txt": "文本文件",
		// .md 表示 Markdown 文档。
		".md": "Markdown文档",
	}
	// 如果扩展名在映射表中，就返回对应中文类型。
	if t, ok := typeMapping[ext]; ok {
		return t
	}
	// 如果是不认识的扩展名，就返回类似 CSV文件、JSON文件。
	return strings.ToUpper(ext[1:]) + "文件"
}
