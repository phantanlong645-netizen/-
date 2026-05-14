package pipeline

import (
	"RAG-repository/internal/model"
	"RAG-repository/pkg/es"
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
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

const embeddingWorkerCount = 4

type embeddingJobResult struct {
	index int
	doc   model.EsDocument
	err   error
}

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

	log.Info("[Processor] 步骤4: 开始并发向量化分块")

	esDocs, err := p.buildEsDocumentsWithConcurrentEmbedding(ctx, dbVectors)
	if err != nil {
		return err
	}

	log.Infof("[Processor] 步骤4: 所有分块向量化完成，准备批量写入 ES，分块数: %d", len(esDocs))

	if err := es.BulkIndexDocuments(ctx, p.esCfg.IndexName, esDocs); err != nil {
		log.Errorf("[Processor] 批量索引分块到 Elasticsearch 失败, Error: %v", err)
		return fmt.Errorf("批量索引分块到 Elasticsearch 失败: %w", err)
	}

	log.Info("[Processor] 步骤4: 所有分块批量写入 ES 完成")
	log.Infof("[Processor] 文件处理成功完成, FileMD5: %s", task.FileMD5)

	if err := p.uploadRepo.UpdateFileVectorizationStatusByMD5AndUserID(task.FileMD5, task.UserID, model.VectorizationStatusCompleted, ""); err != nil {
		return fmt.Errorf("update vectorization status to completed failed: %w", err)
	}

	return nil

}

func (p *Processor) buildEsDocumentsWithConcurrentEmbedding(ctx context.Context, dbVectors []*model.DocumentVector) ([]model.EsDocument, error) {
	// 如果没有任何数据库分块，直接返回空 ES 文档列表。
	if len(dbVectors) == 0 {
		return []model.EsDocument{}, nil
	}

	// 默认使用固定数量的 embedding worker，避免对 embedding 服务打出无限并发。
	workerCount := embeddingWorkerCount
	// 如果分块数量少于默认 worker 数，就只启动和分块数一样多的 worker，避免空转。
	if len(dbVectors) < workerCount {
		workerCount = len(dbVectors)
	}

	// 创建可取消的上下文；任意一个分块向量化失败时，会取消剩余 worker。
	workerCtx, cancel := context.WithCancel(ctx)
	// 函数退出时释放 context 资源。
	defer cancel()

	// jobs 只传分块下标，worker 根据下标回到 dbVectors 里取真实分块。
	jobs := make(chan int)
	// results 用来收集 worker 处理结果；缓冲区设为分块数，避免 worker 发送结果时被长期阻塞。
	results := make(chan embeddingJobResult, len(dbVectors))
	// esDocs 是固定长度结果数组，用 result.index 写回，保证并发完成顺序不影响最终 chunk 顺序。
	esDocs := make([]model.EsDocument, len(dbVectors))

	// wg 用来等待所有 worker 退出。
	var wg sync.WaitGroup
	// 启动固定数量的 worker。
	for workerID := 0; workerID < workerCount; workerID++ {
		// 每启动一个 worker，等待计数加 1。
		wg.Add(1)
		// 启动一个 goroutine 处理 jobs 里的分块下标。
		go func(id int) {
			// 当前 worker 退出时通知 WaitGroup。
			defer wg.Done()

			// 持续从 jobs channel 读取分块下标，直到 jobs 被关闭。
			for index := range jobs {
				// 每处理一个 job 前先检查上下文是否已经取消。
				select {
				// 如果有其他 worker 失败并调用 cancel，这里直接退出。
				case <-workerCtx.Done():
					return
				// 没有取消就继续处理当前分块。
				default:
				}

				// 根据分块下标取出对应的数据库分块。
				docVector := dbVectors[index]
				// 记录当前 worker 正在处理哪个分块。
				log.Infof("[Processor] worker %d 正在向量化分块 %d/%d, ChunkID: %d", id, index+1, len(dbVectors), docVector.ChunkID)

				// 调用 embedding 服务，把当前分块文本转换成向量。
				vector, err := p.embeddingClient.CreateEmbedding(workerCtx, docVector.TextContent)
				// 如果当前分块向量化失败，整个文件处理应失败。
				if err != nil {
					// 记录失败的 worker 和 chunk。
					log.Errorf("[Processor] worker %d 分块 %d 向量化失败, Error: %v", id, docVector.ChunkID, err)
					// 把错误结果发给主 goroutine。
					results <- embeddingJobResult{
						// index 用来告诉主 goroutine 是哪个原始分块失败。
						index: index,
						// err 带上 chunkID，方便日志和接口错误定位。
						err: fmt.Errorf("块 %d 向量化失败: %w", docVector.ChunkID, err),
					}
					// 取消其他 worker，避免继续处理剩余分块。
					cancel()
					// 当前 worker 结束。
					return
				}

				// 当前分块向量化成功，把 ES 文档结果发给主 goroutine。
				results <- embeddingJobResult{
					// index 是原始分块下标，用于按固定位置写回 esDocs。
					index: index,
					// doc 是准备写入 Elasticsearch 的分块文档。
					doc: model.EsDocument{
						// VectorID 作为 ES document ID，保证重试时同一 chunk 会覆盖而不是重复。
						VectorID: fmt.Sprintf("%s_%d", docVector.FileMD5, docVector.ChunkID),
						// FileMD5 标识当前 chunk 属于哪个原始文件。
						FileMD5: docVector.FileMD5,
						// ChunkID 是当前文本分块编号。
						ChunkID: docVector.ChunkID,
						// TextContent 是当前分块原文。
						TextContent: docVector.TextContent,
						// Vector 是 embedding 服务返回的向量。
						Vector: vector,
						// ModelVersion 记录当前使用的 embedding 模型。
						ModelVersion: p.embeddingCfg.Model,
						// UserID 用于后续搜索权限过滤。
						UserID: docVector.UserID,
						// OrgTag 用于后续组织权限过滤。
						OrgTag: docVector.OrgTag,
						// IsPublic 用于后续公开文档权限过滤。
						IsPublic: docVector.IsPublic,
					},
				}
			}
		}(workerID + 1)
	}

	// 单独启动一个 goroutine 负责把所有分块下标投递到 jobs。
	go func() {
		// 所有下标投递完成后关闭 jobs，worker 的 for range 才能自然结束。
		defer close(jobs)
		// 遍历所有数据库分块下标。
		for index := range dbVectors {
			// 每次投递前都监听 workerCtx，避免失败后继续塞任务。
			select {
			// 如果上下文已经取消，说明某个 worker 已经失败，直接停止投递。
			case <-workerCtx.Done():
				return
			// 把当前分块下标发送给某个空闲 worker。
			case jobs <- index:
			}
		}
	}()

	// 单独启动一个 goroutine 负责等待所有 worker 结束。
	go func() {
		// 等待所有 worker 调用 Done。
		wg.Wait()
		// worker 全部退出后关闭 results，主 goroutine 的 for range 才能结束。
		close(results)
	}()

	// completed 记录已经成功收集到的向量化结果数量。
	completed := 0
	// 持续读取 worker 返回的结果，直到 results 被关闭。
	for result := range results {
		// 如果任意 worker 返回错误，整个文件处理直接失败。
		if result.err != nil {
			return nil, result.err
		}

		// 按原始下标写回结果数组，保证最终 ES 文档顺序和 dbVectors 一致。
		esDocs[result.index] = result.doc
		// 成功数量加 1。
		completed++
		// 记录当前完成进度。
		log.Infof("[Processor] 分块向量化完成 %d/%d, ChunkID: %d", completed, len(dbVectors), result.doc.ChunkID)
	}

	// 如果 results 关闭后成功数量还不等于总分块数，说明中途被取消或结果不完整。
	if completed != len(dbVectors) {
		// 如果外层 ctx 已经取消，直接返回外层取消原因。
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// 否则返回一个明确的未完成错误。
		return nil, fmt.Errorf("向量化未完成，已完成 %d/%d", completed, len(dbVectors))
	}

	// 全部分块向量化成功，返回准备批量写入 ES 的文档列表。
	return esDocs, nil
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
