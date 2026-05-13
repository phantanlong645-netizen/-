package pipeline

import (
	"RAG-repository/internal/model"
	"RAG-repository/pkg/es"
	"bytes"
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"RAG-repository/internal/config"
	"RAG-repository/internal/repository"
	"RAG-repository/internal/storagepath"
	"RAG-repository/pkg/embedding"
	"RAG-repository/pkg/log"
	"RAG-repository/pkg/storage"
	"RAG-repository/pkg/tasks"
	"RAG-repository/pkg/tika"

	"github.com/minio/minio-go/v7"
)

type Processor struct {
	tikaClient      *tika.Client
	embeddingClient embedding.Client
	esCfg           config.ElasticsearchConfig
	minioCfg        config.MinIOConfig
	embeddingCfg    config.EmbeddingConfig
	uploadRepo      repository.UploadRepository
	docVectorRepo   repository.DocumentVectorRepository
}

func NewProcessor(
	tikaClient *tika.Client,
	embeddingClient embedding.Client,
	esCfg config.ElasticsearchConfig,
	minioCfg config.MinIOConfig,
	embeddingCfg config.EmbeddingConfig,
	uploadRepo repository.UploadRepository,
	docVectorRepo repository.DocumentVectorRepository,
) *Processor {
	return &Processor{
		tikaClient:      tikaClient,
		embeddingClient: embeddingClient,
		esCfg:           esCfg,
		minioCfg:        minioCfg,
		embeddingCfg:    embeddingCfg,
		uploadRepo:      uploadRepo,
		docVectorRepo:   docVectorRepo,
	}
}

func (p *Processor) Process(ctx context.Context, task tasks.FileProcessingTask) error {
	log.Infof("[Processor] 开始处理文件, FileMD5: %s, FileName: %s, UserID: %d", task.FileMD5, task.FileName, task.UserID)

	if err := p.uploadRepo.UpdateFileVectorizationStatusByMD5AndUserID(task.FileMD5, task.UserID, model.VectorizationStatusProcessing, ""); err != nil {
		return fmt.Errorf("update vectorization status to processing failed: %w", err)
	}

	objectName := storagepath.MergedObjectName(task.UserID, task.FileMD5, task.FileName)
	log.Infof("[Processor] 步骤1: 从MinIO下载文件, Bucket: %s, Object: %s", p.minioCfg.BucketName, objectName)

	object, err := storage.MinioClient.GetObject(ctx, p.minioCfg.BucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		log.Errorf("[Processor] 从MinIO下载文件失败, Object: %s, Error: %v", objectName, err)
		return fmt.Errorf("从 MinIO 下载文件失败: %w", err)
	}
	defer object.Close()

	buf := new(bytes.Buffer)
	size, err := buf.ReadFrom(object)
	if err != nil {
		log.Errorf("[Processor] 从MinIO对象流中读取内容到缓冲区失败, Error: %v", err)
		return fmt.Errorf("读取MinIO对象流失败: %w", err)
	}

	log.Infof("[Processor] 步骤1: 文件下载成功, 从MinIO流中读取到的文件大小为: %d字节", size)

	if size == 0 {
		log.Warnf("[Processor] 文件 '%s' 内容为空, 处理中止", task.FileName)
		return errors.New("文件内容为空")
	}

	log.Info("[Processor] 步骤2: 使用Tika提取文本内容")

	textContent, err := p.tikaClient.ExtractText(bytes.NewReader(buf.Bytes()), task.FileName)
	if err != nil {
		log.Errorf("[Processor] 使用Tika提取文本失败, FileName: %s, Error: %v", task.FileName, err)
		return fmt.Errorf("使用 Tika 提取文本失败: %w", err)
	}

	if textContent == "" {
		log.Warnf("[Processor] Tika提取的文本内容为空, 处理中止, FileName: %s", task.FileName)
		return errors.New("提取的文本内容为空")
	}

	log.Infof("[Processor] 步骤2: 文本提取成功, 内容长度: %d 字符", utf8.RuneCountInString(textContent))

	log.Info("[Processor] 步骤3: 进行文本分块, chunkSize: 1000, chunkOverlap: 100")

	chunks := p.splitText(textContent, 1000, 100)

	log.Infof("[Processor] 步骤3: 文本分块完成, 共生成 %d 个分块", len(chunks))

	if len(chunks) == 0 {
		log.Warnf("[Processor] 未生成任何文本分块, 处理中止, FileName: %s", task.FileName)
		return errors.New("未生成任何文本分块")
	}

	log.Info("[Processor] 阶段一: 开始将分块文本存入数据库")

	dbVectors := make([]*model.DocumentVector, 0, len(chunks))

	for i, chunk := range chunks {
		dbVectors = append(dbVectors, &model.DocumentVector{
			FileMD5:     task.FileMD5,
			ChunkID:     i,
			TextContent: chunk,
			UserID:      task.UserID,
			OrgTag:      task.OrgTag,
			IsPublic:    task.IsPublic,
		})
	}

	if err := p.docVectorRepo.ReplaceByFileMD5(task.FileMD5, dbVectors); err != nil {
		log.Errorf("[Processor] 阶段一: 批量保存文本分块到数据库失败, Error: %v", err)
		return fmt.Errorf("批量保存文本分块失败: %w", err)
	}

	log.Infof("[Processor] 阶段一: 成功将 %d 个分块存入数据库", len(dbVectors))

	log.Info("[Processor] 阶段二: 开始使用已保存的分块进行向量化")

	log.Info("[Processor] 步骤4: 开始遍历分块并进行向量化与索引")

	for i, docVector := range dbVectors {
		log.Infof("[Processor] 正在处理分块 %d/%d, ChunkID: %d", i+1, len(dbVectors), docVector.ChunkID)

		vector, err := p.embeddingClient.CreateEmbedding(ctx, docVector.TextContent)
		if err != nil {
			log.Errorf("[Processor] 分块 %d 向量化失败, Error: %v", docVector.ChunkID, err)
			return fmt.Errorf("块 %d 向量化失败: %w", docVector.ChunkID, err)
		}

		esDoc := model.EsDocument{
			VectorID:     fmt.Sprintf("%s_%d", docVector.FileMD5, docVector.ChunkID),
			FileMD5:      docVector.FileMD5,
			ChunkID:      docVector.ChunkID,
			TextContent:  docVector.TextContent,
			Vector:       vector,
			ModelVersion: p.embeddingCfg.Model,
			UserID:       docVector.UserID,
			OrgTag:       docVector.OrgTag,
			IsPublic:     docVector.IsPublic,
		}

		log.Infof("[Processor] 准备索引到ES的文档 (ChunkID: %d): %+v", esDoc.ChunkID, esDoc)

		if err := es.IndexDocument(ctx, p.esCfg.IndexName, esDoc); err != nil {
			log.Errorf("[Processor] 索引分块 %d 到Elasticsearch失败, Error: %v", docVector.ChunkID, err)
			return fmt.Errorf("索引块 %d 到 Elasticsearch 失败: %w", docVector.ChunkID, err)
		}

		log.Infof("[Processor] 分块 %d/%d 向量化并索引成功", i+1, len(dbVectors))
	}

	log.Info("[Processor] 步骤4: 所有分块处理完毕")
	log.Infof("[Processor] 文件处理成功完成, FileMD5: %s", task.FileMD5)

	if err := p.uploadRepo.UpdateFileVectorizationStatusByMD5AndUserID(task.FileMD5, task.UserID, model.VectorizationStatusCompleted, ""); err != nil {
		return fmt.Errorf("update vectorization status to completed failed: %w", err)
	}

	return nil

}
func (p *Processor) splitText(text string, chunkSize int, chunkOverlap int) []string {
	// 如果分块大小小于等于重叠大小，step 会小于等于 0，无法正常向前推进。
	if chunkSize <= chunkOverlap {
		// 这种异常配置下退回简单切分：只按 chunkSize 切，不做 overlap。
		return p.simpleSplit(text, chunkSize)
	}

	// 保存最终切出来的文本块。
	var chunks []string
	// 把字符串转成 rune 切片，按 Unicode 字符切分，避免按字节切坏中文。
	runes := []rune(text)
	// 空文本没有可切分内容，直接返回 nil。
	if len(runes) == 0 {
		return nil
	}

	// 每次窗口向前移动的步长；比如 chunkSize=1000、overlap=100，则每次前进 900 个字符。
	step := chunkSize - chunkOverlap

	// 从文本开头开始，用滑动窗口不断截取 chunk。
	for i := 0; i < len(runes); i += step {
		// 当前 chunk 的结束位置，默认取 chunkSize 个字符。
		end := i + chunkSize
		// 如果结束位置超过文本长度，就截到文本末尾，避免越界。
		if end > len(runes) {
			end = len(runes)
		}

		// 把当前窗口内的 rune 转回字符串，并加入结果列表。
		chunks = append(chunks, string(runes[i:end]))

		// 如果已经切到文本末尾，说明没有剩余内容，结束循环。
		if end == len(runes) {
			break
		}
	}

	// 返回所有切好的文本块。
	return chunks
}

func (p *Processor) simpleSplit(text string, chunkSize int) []string {
	var chunks []string
	runes := []rune(text)

	if len(runes) == 0 {
		return nil
	}

	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}

		chunks = append(chunks, string(runes[i:end]))
	}

	return chunks
}
