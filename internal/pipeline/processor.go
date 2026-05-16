package pipeline

import (
	"RAG-repository/internal/model"
	"RAG-repository/pkg/es"
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
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

	"github.com/go-ego/gse"
	"github.com/minio/minio-go/v7"
)

const embeddingWorkerCount = 4

const semanticMinChunkSize = 100

var (
	chineseSegmenterOnce sync.Once
	chineseSegmenter     gse.Segmenter
	chineseSegmenterErr  error
)

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

	if err := p.docVectorRepo.ReplaceByFileMD5AndUserID(task.FileMD5, task.UserID, dbVectors); err != nil {
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
						// VectorID 作为 ES document ID，带上 userID，保证同 MD5 不同用户互不覆盖。
						VectorID: fmt.Sprintf("%s_%d_%d", docVector.FileMD5, docVector.UserID, docVector.ChunkID),
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
	// 如果 chunkSize 小于等于 0，说明分块配置无效，直接返回空结果。
	if chunkSize <= 0 {
		// 无效配置下不能继续切分，否则 simpleSplit 也可能无法推进。
		return nil
	}

	// 去掉文本首尾空白，避免开头或结尾生成空 chunk。
	text = strings.TrimSpace(text)
	// 如果清理后没有文本内容，直接返回空结果。
	if text == "" {
		// 空文本没有可用于向量化的内容。
		return nil
	}

	// 先按段落、句子和词/字符兜底生成基础语义 chunk。
	baseChunks := p.splitTextIntoBaseChunks(text, chunkSize)
	// 如果语义切分没有得到任何结果，就退回简单字符切分。
	if len(baseChunks) == 0 {
		// 这个兜底避免异常文本格式导致文档完全没有分块。
		return p.simpleSplit(text, chunkSize)
	}

	// 把过短的 chunk 合并到相邻 chunk，减少无效小片段。
	mergedChunks := p.mergeSmallChunks(baseChunks, chunkSize, chunkOverlap)
	// 在相邻 chunk 之间补语义 overlap，让跨块上下文更完整。
	return p.addSemanticOverlap(mergedChunks, chunkOverlap)
}

func (p *Processor) splitTextIntoBaseChunks(text string, chunkSize int) []string {
	// chunks 保存按语义边界切出来的基础分块。
	var chunks []string
	// paragraphs 按空行切段落，优先保留自然段完整性。
	paragraphs := p.splitParagraphs(text)
	// current 保存当前正在累积的 chunk。
	var current string

	// 遍历每一个段落。
	for _, paragraph := range paragraphs {
		// 清理段落首尾空白。
		paragraph = strings.TrimSpace(paragraph)
		// 跳过空段落。
		if paragraph == "" {
			// 空段落不参与切分。
			continue
		}

		// 如果单个段落已经超过 chunkSize，就进一步按句子切。
		if p.runeLen(paragraph) > chunkSize {
			// 当前 chunk 里已有内容时，先把它落盘到结果。
			if strings.TrimSpace(current) != "" {
				// 保存当前累积 chunk。
				chunks = append(chunks, strings.TrimSpace(current))
				// 清空 current，避免和长段落混在一起。
				current = ""
			}
			// 把长段落拆成更小的句子级 chunk。
			chunks = append(chunks, p.splitLongParagraph(paragraph, chunkSize)...)
			// 长段落处理完后继续下一个段落。
			continue
		}

		// candidate 是尝试把当前段落合并进 current 后的文本。
		candidate := p.combineChunks(current, paragraph)
		// 如果合并后超过 chunkSize，就先保存 current，再用当前段落开新 chunk。
		if current != "" && p.runeLen(candidate) > chunkSize {
			// 保存当前 chunk。
			chunks = append(chunks, strings.TrimSpace(current))
			// 当前段落作为新 chunk 的起点。
			current = paragraph
			// 当前段落已经处理完成。
			continue
		}

		// 如果没有超长，就把合并结果作为当前 chunk。
		current = candidate
	}

	// 循环结束后，如果 current 还有内容，需要补进结果。
	if strings.TrimSpace(current) != "" {
		// 保存最后一个 chunk。
		chunks = append(chunks, strings.TrimSpace(current))
	}

	// 返回基础语义 chunk。
	return chunks
}

func (p *Processor) splitLongParagraph(paragraph string, chunkSize int) []string {
	// chunks 保存长段落按句子切出来的结果。
	var chunks []string
	// sentences 按中英文句号、问号、感叹号和分号切句子。
	sentences := p.splitSentences(paragraph)
	// current 保存当前正在累积的句子 chunk。
	var current string

	// 遍历长段落里的每个句子。
	for _, sentence := range sentences {
		// 清理句子首尾空白。
		sentence = strings.TrimSpace(sentence)
		// 跳过空句子。
		if sentence == "" {
			// 空句子不参与切分。
			continue
		}

		// 如果单句本身超过 chunkSize，就继续按词或字符兜底切。
		if p.runeLen(sentence) > chunkSize {
			// 当前 chunk 里已有内容时先保存。
			if strings.TrimSpace(current) != "" {
				// 保存当前句子 chunk。
				chunks = append(chunks, strings.TrimSpace(current))
				// 清空 current。
				current = ""
			}
			// 拆分超长句子。
			chunks = append(chunks, p.splitLongSentence(sentence, chunkSize)...)
			// 超长句子处理完后继续下一个句子。
			continue
		}

		// candidate 是尝试把当前句子拼进 current 后的文本。
		candidate := current + sentence
		// 如果拼接后超过 chunkSize，就保存 current，再从当前句子开新 chunk。
		if current != "" && p.runeLen(candidate) > chunkSize {
			// 保存当前 chunk。
			chunks = append(chunks, strings.TrimSpace(current))
			// 当前句子作为新 chunk 起点。
			current = sentence
			// 当前句子已经处理完成。
			continue
		}

		// 如果没有超长，就把句子拼进 current。
		current = candidate
	}

	// 循环结束后保存最后一个句子 chunk。
	if strings.TrimSpace(current) != "" {
		// 保存最后的 current。
		chunks = append(chunks, strings.TrimSpace(current))
	}

	// 返回长段落切分结果。
	return chunks
}

func (p *Processor) splitLongSentence(sentence string, chunkSize int) []string {
	// 如果句子里有空白分隔，优先按空白词边界切英文或混合文本。
	if strings.ContainsAny(sentence, " \t\r\n") {
		// chunks 保存按词边界切出来的结果。
		var chunks []string
		// words 提取连续非空白内容和其后空白，尽量保留原始间隔。
		words := regexp.MustCompile(`\S+\s*`).FindAllString(sentence, -1)
		// current 保存当前正在累积的词块。
		var current string

		// 遍历所有词。
		for _, word := range words {
			// 如果单个词超过 chunkSize，就用字符兜底切。
			if p.runeLen(word) > chunkSize {
				// current 有内容时先保存。
				if strings.TrimSpace(current) != "" {
					// 保存当前词块。
					chunks = append(chunks, strings.TrimSpace(current))
					// 清空 current。
					current = ""
				}
				// 对超长词做字符切分。
				chunks = append(chunks, p.simpleSplit(strings.TrimSpace(word), chunkSize)...)
				// 当前词处理完，继续下一个词。
				continue
			}

			// candidate 是尝试加入当前词后的文本。
			candidate := current + word
			// 如果加入当前词会超长，就先保存 current，再开启新 chunk。
			if current != "" && p.runeLen(candidate) > chunkSize {
				// 保存当前词块。
				chunks = append(chunks, strings.TrimSpace(current))
				// 当前词作为新 chunk 起点。
				current = word
				// 当前词已经处理完成。
				continue
			}

			// 如果没有超长，就继续累积。
			current = candidate
		}

		// 保存最后一个词块。
		if strings.TrimSpace(current) != "" {
			// 把尾部 current 加入结果。
			chunks = append(chunks, strings.TrimSpace(current))
		}

		// 如果词边界切分有结果，就返回。
		if len(chunks) > 0 {
			// 返回按词边界切出来的 chunk。
			return chunks
		}
	}

	// 没有明显空白词边界时，优先按中文分词切长句，避免中文只能按单字切。
	return p.splitChineseSentence(sentence, chunkSize)
}

func (p *Processor) splitChineseSentence(sentence string, chunkSize int) []string {
	// chineseSegmenterOnce 保证中文分词器在进程内只初始化一次，避免每个分片重复加载词典。
	chineseSegmenterOnce.Do(func() {
		// NewEmbed("zh") 使用 gse 内置中文词典，不依赖外部词典文件。
		chineseSegmenter, chineseSegmenterErr = gse.NewEmbed("zh")
	})

	// 如果分词器初始化失败，就退回到 rune 字符切分，保证处理流程不中断。
	if chineseSegmenterErr != nil {
		// 记录告警，方便定位词典或依赖初始化问题。
		log.Warnf("[Processor] 中文分词器初始化失败，使用字符切分兜底。error: %v", chineseSegmenterErr)
		// 返回按 rune 切分的结果，避免中文被按字节截断。
		return p.simpleSplit(sentence, chunkSize)
	}

	// words 是中文分词结果；第二个参数 true 表示启用 HMM，提升未登录词识别能力。
	words := chineseSegmenter.Cut(sentence, true)
	// 如果没有切出任何词，就退回到 rune 字符切分。
	if len(words) == 0 {
		// 返回字符级兜底结果。
		return p.simpleSplit(sentence, chunkSize)
	}

	// chunks 保存按中文词边界组合出来的文本块。
	var chunks []string
	// current 保存当前正在累积的文本块。
	var current string

	// 遍历中文分词结果。
	for _, word := range words {
		// 清理词两侧空白；中文词一般没有空白，英文混排时可能会有。
		word = strings.TrimSpace(word)
		// 跳过空词，避免生成空 chunk。
		if word == "" {
			// 当前词没有业务内容，继续处理下一个词。
			continue
		}

		// 如果单个词本身已经超过 chunkSize，就只能继续按 rune 字符兜底切这个词。
		if p.runeLen(word) > chunkSize {
			// current 已有内容时，先保存当前 chunk。
			if strings.TrimSpace(current) != "" {
				// 保存当前按词累积的 chunk。
				chunks = append(chunks, strings.TrimSpace(current))
				// 清空 current，准备处理超长词。
				current = ""
			}
			// 对超长词做字符级切分，避免一个词撑爆 chunkSize。
			chunks = append(chunks, p.simpleSplit(word, chunkSize)...)
			// 当前词处理结束，继续下一个词。
			continue
		}

		// candidate 是把当前词追加到 current 后的候选 chunk。
		candidate := current + word
		// 如果追加当前词会超过 chunkSize，就先保存 current，再用当前词开启新 chunk。
		if current != "" && p.runeLen(candidate) > chunkSize {
			// 保存已经达到长度上限附近的 chunk。
			chunks = append(chunks, strings.TrimSpace(current))
			// 当前词作为新 chunk 的开头。
			current = word
			// 当前词已经放入新 chunk，继续处理下一个词。
			continue
		}

		// 没有超长时，继续累积当前词。
		current = candidate
	}

	// 循环结束后，如果 current 还有内容，保存最后一个 chunk。
	if strings.TrimSpace(current) != "" {
		// 保存尾部 chunk。
		chunks = append(chunks, strings.TrimSpace(current))
	}

	// 如果中文分词组合出了结果，就返回这些 chunk。
	if len(chunks) > 0 {
		// 返回按中文词边界切出来的结果。
		return chunks
	}

	// 理论上不会走到这里；作为兜底，仍然按 rune 字符切分。
	return p.simpleSplit(sentence, chunkSize)
}

func (p *Processor) mergeSmallChunks(chunks []string, chunkSize int, chunkOverlap int) []string {
	// merged 保存合并小块后的结果。
	var merged []string
	// minSize 是最小 chunk 长度，过短的 chunk 会尽量和前一个合并。
	minSize := semanticMinChunkSize
	// 如果 chunkSize 比默认最小值还小，就以 chunkSize 作为最小值。
	if chunkSize < minSize {
		// 避免 minSize 大于 chunkSize 导致所有块都被认为过小。
		minSize = chunkSize
	}
	// maxMergedSize 允许小块合并后略超过 chunkSize，和 Java 的 chunkSize + overlap 类似。
	maxMergedSize := chunkSize + p.normalizedOverlapSize(chunkSize, chunkOverlap)

	// 遍历基础 chunk。
	for _, chunk := range chunks {
		// 清理 chunk 首尾空白。
		chunk = strings.TrimSpace(chunk)
		// 跳过空 chunk。
		if chunk == "" {
			// 空 chunk 不参与合并。
			continue
		}

		// 如果已有前一个 chunk，就尝试合并小块。
		if len(merged) > 0 {
			// previous 是上一个已经保存的 chunk。
			previous := merged[len(merged)-1]
			// combined 是把前一个 chunk 和当前 chunk 按段落方式合并后的文本。
			combined := p.combineChunks(previous, chunk)
			// 如果当前或前一个过短，并且合并后不超过上限，就合并。
			if (p.runeLen(chunk) < minSize || p.runeLen(previous) < minSize) && p.runeLen(combined) <= maxMergedSize {
				// 用合并后的文本替换最后一个 chunk。
				merged[len(merged)-1] = combined
				// 当前 chunk 已经被合并，不再单独追加。
				continue
			}
		}

		// 无法合并时，当前 chunk 单独加入结果。
		merged = append(merged, chunk)
	}

	// 返回合并后的 chunk。
	return merged
}

func (p *Processor) addSemanticOverlap(chunks []string, chunkOverlap int) []string {
	// 如果 overlap 配置无效，直接返回原始 chunks。
	if chunkOverlap <= 0 {
		// 不做任何重叠处理。
		return chunks
	}
	// 如果只有 0 或 1 个 chunk，不需要加 overlap。
	if len(chunks) <= 1 {
		// 没有相邻 chunk 时无重叠意义。
		return chunks
	}

	// overlapped 保存补充语义 overlap 后的结果。
	overlapped := make([]string, 0, len(chunks))
	// 第一个 chunk 没有上一个 chunk，所以原样保留。
	overlapped = append(overlapped, chunks[0])

	// 从第二个 chunk 开始补前文 overlap。
	for i := 1; i < len(chunks); i++ {
		// overlapText 从上一个 chunk 末尾按句子边界取上下文。
		overlapText := p.buildOverlapText(chunks[i-1], chunkOverlap)
		// 如果没有可用 overlap，就原样追加当前 chunk。
		if overlapText == "" {
			// 原样追加当前 chunk。
			overlapped = append(overlapped, chunks[i])
			// 继续处理下一个 chunk。
			continue
		}

		// 有 overlap 时，把 overlap 放在当前 chunk 前面。
		overlapped = append(overlapped, overlapText+"\n\n"+chunks[i])
	}

	// 返回带语义 overlap 的 chunks。
	return overlapped
}

func (p *Processor) buildOverlapText(text string, maxLength int) string {
	// 清理输入文本。
	text = strings.TrimSpace(text)
	// 如果文本为空或 overlap 长度无效，返回空字符串。
	if text == "" || maxLength <= 0 {
		// 没有可用 overlap。
		return ""
	}

	// sentences 把上一个 chunk 拆成句子单元。
	sentences := p.splitSentences(text)
	// overlap 保存最终取出的句子级重叠文本。
	overlap := ""

	// 从最后一句往前取，尽量保持句子完整。
	for i := len(sentences) - 1; i >= 0; i-- {
		// sentence 是当前候选句子。
		sentence := strings.TrimSpace(sentences[i])
		// 跳过空句子。
		if sentence == "" {
			// 空句子不进入 overlap。
			continue
		}

		// 如果单句超过 maxLength，只取它的尾部。
		if p.runeLen(sentence) > maxLength {
			// 如果 overlap 还为空，就用长句尾部作为 overlap。
			if overlap == "" {
				// 返回长句尾部。
				return p.tailByRuneBoundary(sentence, maxLength)
			}
			// 如果已经有 overlap，就保留已有完整句子。
			return strings.TrimSpace(overlap)
		}

		// candidate 是把当前句子放到已有 overlap 前面的结果。
		candidate := sentence + overlap
		// 如果超过最大 overlap 长度，就停止继续往前取。
		if p.runeLen(candidate) > maxLength {
			// 已经达到长度上限。
			break
		}

		// 更新 overlap。
		overlap = candidate
	}

	// 如果没取到完整句子，就按字符尾部兜底。
	if strings.TrimSpace(overlap) == "" {
		// 返回文本尾部。
		return p.tailByRuneBoundary(text, maxLength)
	}

	// 返回清理后的 overlap。
	return strings.TrimSpace(overlap)
}

func (p *Processor) splitParagraphs(text string) []string {
	// paragraphRegexp 匹配一个或多个空行，用于识别段落边界。
	paragraphRegexp := regexp.MustCompile(`\n\s*\n+`)
	// 按段落边界切分文本。
	return paragraphRegexp.Split(text, -1)
}

func (p *Processor) splitSentences(text string) []string {
	// sentenceRegexp 匹配中英文常见句子结束符，尽量保留结束标点。
	sentenceRegexp := regexp.MustCompile(`[^。！？；.!?;]+[。！？；.!?;]?`)
	// sentences 保存匹配出的句子。
	sentences := sentenceRegexp.FindAllString(text, -1)
	// 如果正则没有匹配到句子，就把原文本作为一个句子返回。
	if len(sentences) == 0 {
		// 返回原文本，避免内容丢失。
		return []string{text}
	}

	// 返回句子列表。
	return sentences
}

func (p *Processor) combineChunks(first string, second string) string {
	// 清理第一个 chunk。
	first = strings.TrimSpace(first)
	// 清理第二个 chunk。
	second = strings.TrimSpace(second)
	// 如果第一个为空，直接返回第二个。
	if first == "" {
		// 返回第二个 chunk。
		return second
	}
	// 如果第二个为空，直接返回第一个。
	if second == "" {
		// 返回第一个 chunk。
		return first
	}

	// 两个 chunk 都有内容时，用空行连接，保留段落感。
	return first + "\n\n" + second
}

func (p *Processor) normalizedOverlapSize(chunkSize int, chunkOverlap int) int {
	// 如果 overlap 配置无效，返回 0。
	if chunkOverlap <= 0 || chunkSize <= 1 {
		// 没有可用 overlap。
		return 0
	}
	// 如果 overlap 大于 chunkSize，就限制到 chunkSize-1，避免超过主体长度。
	if chunkOverlap >= chunkSize {
		// 返回最大安全 overlap。
		return chunkSize - 1
	}
	// 返回原始 overlap。
	return chunkOverlap
}

func (p *Processor) tailByRuneBoundary(text string, maxLength int) string {
	// 把文本转成 rune，避免按字节截断中文。
	runes := []rune(strings.TrimSpace(text))
	// 如果长度本来就不超过上限，直接返回原文本。
	if len(runes) <= maxLength {
		// 返回完整文本。
		return string(runes)
	}
	// 从尾部截取 maxLength 个 rune。
	return string(runes[len(runes)-maxLength:])
}

func (p *Processor) runeLen(text string) int {
	// utf8.RuneCountInString 按 Unicode 字符计数，不按字节计数。
	return utf8.RuneCountInString(text)
}

func (p *Processor) simpleSplit(text string, chunkSize int) []string {
	// 如果 chunkSize 无效，直接返回空结果，避免死循环。
	if chunkSize <= 0 {
		// 无效配置不能继续切分。
		return nil
	}

	// chunks 保存字符兜底切分结果。
	var chunks []string
	// 把字符串转成 rune，避免切坏中文字符。
	runes := []rune(text)

	// 如果文本为空，直接返回空结果。
	if len(runes) == 0 {
		// 没有可切分内容。
		return nil
	}

	// 按 chunkSize 固定长度兜底切分。
	for i := 0; i < len(runes); i += chunkSize {
		// 计算当前 chunk 的结束下标。
		end := i + chunkSize
		// 如果结束下标超过文本长度，就截到文本末尾。
		if end > len(runes) {
			// 修正结束下标，避免越界。
			end = len(runes)
		}

		// 把当前 rune 区间转回字符串并加入结果。
		chunks = append(chunks, string(runes[i:end]))
	}

	// 返回兜底切分结果。
	return chunks
}
