package service

import (
	"RAG-repository/pkg/log"
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"RAG-repository/internal/model"
	"RAG-repository/internal/repository"
	"RAG-repository/pkg/embedding"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/goccy/go-json"
)

// SearchService 定义搜索服务对外暴露的方法。
type SearchService interface {
	// HybridSearch 执行混合搜索：向量召回 + 文本匹配 + 权限过滤。
	HybridSearch(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error)
}

// searchService 是 SearchService 的具体实现。
type searchService struct {
	// embeddingClient 用来把用户搜索词转换成向量。
	embeddingClient embedding.Client
	// esClient 用来访问 Elasticsearch。
	esClient *elasticsearch.Client
	// userService 用来计算当前用户能看的组织标签。
	userService UserService
	// uploadRepo 用来根据 fileMD5 查询文件名等文件元数据。
	uploadRepo repository.UploadRepository
}

// NewSearchService 组装搜索服务需要的依赖。
func NewSearchService(
	// embeddingClient 负责调用向量模型。
	embeddingClient embedding.Client,
	// esClient 负责执行 ES 查询。
	esClient *elasticsearch.Client,
	// userService 负责用户和组织权限逻辑。
	userService UserService,
	// uploadRepo 负责文件上传记录查询。
	uploadRepo repository.UploadRepository,
) SearchService {
	// 返回接口类型，外部只依赖 SearchService 方法，不直接依赖结构体。
	return &searchService{
		// 保存向量客户端。
		embeddingClient: embeddingClient,
		// 保存 ES 客户端。
		esClient: esClient,
		// 保存用户服务。
		userService: userService,
		// 保存上传仓储。
		uploadRepo: uploadRepo,
	}
}

// HybridSearch 执行知识库搜索，并把 ES 文档结果转换成前端需要的 DTO。
func (s *searchService) HybridSearch(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error) {
	// 打印搜索入口日志，方便排查搜索词、返回数量和当前用户。
	log.Infof("[SearchService] 开始执行混合搜索, query: '%s', topK: %d, user: %s", query, topK, user.Username)

	// 计算当前用户可访问的组织标签，用于后续 ES 权限过滤。
	userEffectiveTags, err := s.userService.GetUserEffectiveOrgTags(user)
	// 如果组织标签计算失败，不直接中断搜索，而是降级为空标签列表。
	if err != nil {
		log.Errorf("[SearchService] 获取用户有效组织标签失败: %v", err)
		// 空标签表示后续只能命中本人或公开文档，不能命中组织共享文档。
		userEffectiveTags = []string{}
	}
	log.Infof("[SearchService] 获取到 %d 个有效组织标签: %v", len(userEffectiveTags), userEffectiveTags)

	// 规范化搜索词，并提取一个更适合短语匹配的 phrase。
	normalized, phrase := normalizeQuery(query)
	// 如果规范化后的搜索词发生变化，记录一下便于分析召回效果。
	if normalized != query {
		log.Infof("[SearchService] 规范化查询: '%s' -> '%s' (phrase='%s')", query, normalized, phrase)
	}

	// 把原始搜索词转换成向量，用于 ES 的 kNN 向量召回。
	queryVector, err := s.embeddingClient.CreateEmbedding(ctx, query)
	// 向量生成失败时，搜索无法继续，因为后面的 knn 查询依赖这个向量。
	if err != nil {
		log.Errorf("[SearchService] 向量化查询失败: %v", err)
		// 把底层错误包装后返回给上层。
		return nil, fmt.Errorf("failed to create query embedding: %w", err)
	}
	log.Infof("[SearchService] 向量化查询成功, 向量维度: %d", len(queryVector))
	// buf 用来承载编码后的 Elasticsearch JSON 请求体。
	var buf bytes.Buffer

	// esQuery 是发送给 Elasticsearch 的混合检索 DSL。
	esQuery := map[string]interface{}{
		// knn 部分负责按向量相似度召回候选片段。
		"knn": map[string]interface{}{
			// field 指定 ES 文档里保存 embedding 向量的字段。
			"field": "vector",
			// query_vector 是当前搜索词生成出来的向量。
			"query_vector": queryVector,
			// k 控制向量召回数量，这里放大 topK 给后续重排留空间。
			"k": topK * 30,
			// num_candidates 控制近似向量检索参与计算的候选数量。
			"num_candidates": topK * 30,
		},
		// query 部分负责文本匹配和权限过滤。
		"query": map[string]interface{}{
			// bool 查询可以组合 must、filter、should 等条件。
			"bool": map[string]interface{}{
				// must 表示必须满足的文本匹配条件。
				"must": map[string]interface{}{
					// match 对分词后的正文内容做全文检索。
					"match": map[string]interface{}{
						// text_content 是知识片段正文，normalized 是规范化后的搜索词。
						"text_content": normalized,
					},
				},
				// filter 不参与打分，只负责控制哪些文档可见。
				"filter": map[string]interface{}{
					// 这里用 bool should 表示三种权限条件满足任意一种即可。
					"bool": map[string]interface{}{
						// should 里放本人文档、公开文档、组织共享文档三类可见条件。
						"should": []map[string]interface{}{
							// 当前用户自己上传的文档可见。
							{"term": map[string]interface{}{"user_id": user.ID}},
							// 公开文档可见。
							{"term": map[string]interface{}{"is_public": true}},
							// 当前用户组织标签命中的文档可见。
							{"terms": map[string]interface{}{"org_tag": userEffectiveTags}},
						},
						// 至少命中一个 should 条件才算有权限。
						"minimum_should_match": 1,
					},
				},
				// should 用短语匹配提高精准命中文档的分数。
				"should": buildPhraseShould(phrase),
			},
		},
		// rescore 对初步召回的候选结果进行二次排序。
		"rescore": map[string]interface{}{
			// window_size 表示参与二次排序的候选窗口大小。
			"window_size": topK * 30,
			// query 定义二次排序规则。
			"query": map[string]interface{}{
				// rescore_query 是重排时额外执行的查询。
				"rescore_query": map[string]interface{}{
					// 用 match 重新检查正文是否包含规范化查询词。
					"match": map[string]interface{}{
						// 对 text_content 做更严格的 and 匹配。
						"text_content": map[string]interface{}{
							// query 是规范化后的搜索词。
							"query": normalized,
							// operator=and 表示分词后尽量要求所有词都出现。
							"operator": "and",
						},
					},
				},
				// 原始查询分数权重较低。
				"query_weight": 0.2,
				// 重排查询分数权重较高。
				"rescore_query_weight": 1.0,
			},
		},
		// size 控制最终返回给业务层的条数。
		"size": topK,
	}

	// 把查询 DSL 编码成 JSON，请求体会写入 buf。
	if err := json.NewEncoder(&buf).Encode(esQuery); err != nil {
		log.Errorf("[SearchService] 序列化 Elasticsearch 查询失败: %v", err)
		// JSON 编码失败通常说明查询结构里有不能被序列化的值。
		return nil, fmt.Errorf("failed to encode es query: %w", err)
	}

	log.Infof("[SearchService] 构建的 Elasticsearch 查询语句: %s", buf.String())

	// 调用 Elasticsearch 的 Search API。
	res, err := s.esClient.Search(
		// 透传请求上下文，支持超时和取消。
		s.esClient.Search.WithContext(ctx),
		// knowledge_base 是存放知识片段向量和正文的索引。
		s.esClient.Search.WithIndex("knowledge_base"),
		// 把前面构造好的 JSON 查询体作为请求体。
		s.esClient.Search.WithBody(&buf),
		// 要求 ES 追踪总命中数，方便调试和统计。
		s.esClient.Search.WithTrackTotalHits(true),
	)
	// 请求 ES 本身失败时，直接返回错误。
	if err != nil {
		log.Errorf("[SearchService] 向 Elasticsearch 发送搜索请求失败: %v", err)
		// 包装 ES 客户端错误。
		return nil, fmt.Errorf("elasticsearch search failed: %w", err)
	}
	// 读取完响应后关闭响应体，避免连接泄漏。
	defer res.Body.Close()

	// 如果 ES 返回 4xx/5xx，需要读取响应体帮助定位 DSL 或索引问题。
	if res.IsError() {
		// 读取 ES 返回的错误详情。
		bodyBytes, _ := io.ReadAll(res.Body)
		log.Errorf("[SearchService] Elasticsearch 返回错误, status: %s, body: %s", res.Status(), string(bodyBytes))
		// 把 ES 错误状态返回给调用方。
		return nil, fmt.Errorf("elasticsearch returned an error: %s", res.String())
	}

	log.Info("[SearchService] 成功从 Elasticsearch 获取响应")
	// esResponse 只声明当前业务需要的 ES 响应字段。
	var esResponse struct {
		// Hits 是 ES 响应里的 hits 外层对象。
		Hits struct {
			// Hits 是真正的命中文档数组。
			Hits []struct {
				// Source 是 ES 文档的 _source，映射到业务里的 EsDocument。
				Source model.EsDocument `json:"_source"`
				// Score 是 ES 对该命中文档计算出来的相关性分数。
				Score float64 `json:"_score"`
			} `json:"hits"`
		} `json:"hits"`
	}

	// 解析 ES 返回的 JSON 响应体。
	if err := json.NewDecoder(res.Body).Decode(&esResponse); err != nil {
		log.Errorf("[SearchService] 解析 Elasticsearch 响应失败: %v", err)
		// 响应结构无法解析时，返回解码错误。
		return nil, fmt.Errorf("failed to decode es response: %w", err)
	}

	// 如果第一次搜索没有命中，尝试用更短的 phrase 再查一次。
	if len(esResponse.Hits.Hits) == 0 {
		log.Infof("[SearchService] Elasticsearch 返回 0 条命中结果")
		// 只有 phrase 非空且确实和原始 query 不同时，才需要重试。
		if phrase != "" && phrase != query {
			log.Infof("[SearchService] 使用核心短语重试查询: '%s'", phrase)

			// retryBuf 保存重试查询的 JSON 请求体。
			var retryBuf bytes.Buffer
			// retryQuery 复用原来的查询结构，只替换文本匹配部分。
			retryQuery := esQuery

			// 把 must match 的搜索词从 normalized 替换成 phrase。
			((retryQuery["query"].(map[string]interface{}))["bool"].(map[string]interface{}))["must"] = map[string]interface{}{
				"match": map[string]interface{}{
					"text_content": phrase,
				},
			}

			// 把 rescore 的搜索词也替换成 phrase，保持重排逻辑一致。
			((retryQuery["rescore"].(map[string]interface{}))["query"].(map[string]interface{}))["rescore_query"] = map[string]interface{}{
				"match": map[string]interface{}{
					"text_content": map[string]interface{}{
						// 重试时使用核心短语。
						"query": phrase,
						// 继续使用 and，提高短语重试的精确度。
						"operator": "and",
					},
				},
			}

			// 把重试查询编码成 JSON；编码失败就跳过重试，不影响主流程。
			if err := json.NewEncoder(&retryBuf).Encode(retryQuery); err == nil {
				// 再次调用 ES 搜索。
				res2, err2 := s.esClient.Search(
					s.esClient.Search.WithContext(ctx),
					s.esClient.Search.WithIndex("knowledge_base"),
					s.esClient.Search.WithBody(&retryBuf),
					s.esClient.Search.WithTrackTotalHits(true),
				)

				// 只有重试请求成功且 ES 没有返回错误状态时，才读取重试结果。
				if err2 == nil && !res2.IsError() {
					// 关闭重试响应体。
					defer res2.Body.Close()

					// 用重试结果覆盖 esResponse。
					if err := json.NewDecoder(res2.Body).Decode(&esResponse); err == nil {
						log.Infof("[SearchService] 重试后命中 %d 条", len(esResponse.Hits.Hits))
					}
				}
			}
		}

		// 重试后仍然没有命中，则返回空数组，不返回 nil，方便前端直接遍历。
		if len(esResponse.Hits.Hits) == 0 {
			return []model.SearchResponseDTO{}, nil
		}
	}

	// fileMD5s 保存 ES 命中片段对应的文件 MD5，后面用它查文件名。
	fileMD5s := make([]string, 0, len(esResponse.Hits.Hits))
	// 遍历所有命中片段。
	for _, hit := range esResponse.Hits.Hits {
		// 收集每个片段所在文件的 MD5。
		fileMD5s = append(fileMD5s, hit.Source.FileMD5)
	}

	// uniqueMD5s 用 map 去重，避免同一个文件被重复查询。
	uniqueMD5s := make(map[string]struct{})
	// 遍历原始 MD5 列表。
	for _, md5 := range fileMD5s {
		// 空结构体不占额外业务含义，只表示这个 MD5 出现过。
		uniqueMD5s[md5] = struct{}{}
	}

	// md5List 是去重后的 MD5 切片，方便传给仓储批量查询。
	md5List := make([]string, 0, len(uniqueMD5s))
	// 遍历去重 map。
	for md5 := range uniqueMD5s {
		// 把 map key 转成切片元素。
		md5List = append(md5List, md5)
	}

	// 根据命中的文件 MD5 批量查询文件上传记录，主要是拿文件名。
	fileInfos, err := s.uploadRepo.FindBatchByMD5s(md5List)
	// 文件元数据查询失败时，搜索结果无法完整展示文件名，因此返回错误。
	if err != nil {
		log.Errorf("[SearchService] 批量查询文件信息失败: %v", err)
		return nil, fmt.Errorf("批量查询文件信息失败: %w", err)
	}

	// fileNameMap 建立 fileMD5 -> fileName 的映射，方便后面 O(1) 取文件名。
	fileNameMap := make(map[string]string)
	// 遍历数据库查到的文件记录。
	for _, info := range fileInfos {
		// 以文件 MD5 为 key 保存文件名。
		fileNameMap[info.FileMD5] = info.FileName
	}

	// results 是最终返回给前端的搜索结果 DTO 列表。
	results := make([]model.SearchResponseDTO, 0, len(esResponse.Hits.Hits))
	// 遍历每一条 ES 命中片段。
	for _, hit := range esResponse.Hits.Hits {
		// 根据片段的 fileMD5 找到对应的文件名。
		fileName := fileNameMap[hit.Source.FileMD5]
		// 如果数据库里没有找到文件名，使用兜底名称。
		if fileName == "" {
			log.Warnf("[SearchService] 未找到 FileMD5 '%s' 对应的文件名，将使用 '未知文件'", hit.Source.FileMD5)
			fileName = "未知文件"
		}

		// 把 ES 文档结构转换成前端接口需要的 SearchResponseDTO。
		dto := model.SearchResponseDTO{
			// FileMD5 标识片段所属文件。
			FileMD5: hit.Source.FileMD5,
			// FileName 是从上传记录里回填的文件名。
			FileName: fileName,
			// ChunkID 是文本片段编号。
			ChunkID: hit.Source.ChunkID,
			// TextContent 是命中的文本片段内容。
			TextContent: hit.Source.TextContent,
			// Score 是 ES 返回的相关性分数。
			Score: hit.Score,
			// UserID 转成字符串，匹配 DTO 字段类型。
			UserID: strconv.FormatUint(uint64(hit.Source.UserID), 10),
			// OrgTag 是片段所属组织标签。
			OrgTag: hit.Source.OrgTag,
			// IsPublic 表示片段所属文件是否公开。
			IsPublic: hit.Source.IsPublic,
		}

		// 把当前 DTO 加入最终结果列表。
		results = append(results, dto)
	}

	log.Infof("[SearchService] 组装最终响应成功，返回 %d 条结果", len(results))
	return results, nil

}

// normalizeQuery 清洗用户搜索词，并返回用于普通匹配和短语匹配的文本。
func normalizeQuery(q string) (string, string) {
	// 空搜索词直接返回，phrase 也为空。
	if q == "" {
		return q, ""
	}

	// 转小写，方便英文搜索词统一匹配。
	lower := strings.ToLower(q)

	// stopPhrases 是一些对检索帮助不大的问句词、语气词和限制词。
	stopPhrases := []string{
		"是谁", "是什么", "是啥", "请问", "怎么", "如何",
		"告诉我", "严格", "按照", "不要补充", "的区别",
		"区别", "吗", "呢", "？", "?",
	}

	// 遍历停用短语。
	for _, sp := range stopPhrases {
		// 把停用短语替换成空格，避免词粘在一起。
		lower = strings.ReplaceAll(lower, sp, " ")
	}

	// reKeep 只保留中文、英文数字和空白字符。
	reKeep := regexp.MustCompile(`[^\p{Han}a-z0-9\s]+`)
	// 把其他符号统一替换成空格。
	kept := reKeep.ReplaceAllString(lower, " ")

	// reSpace 用来合并连续空白。
	reSpace := regexp.MustCompile(`\s+`)
	// 合并多余空白并去掉首尾空白。
	kept = strings.TrimSpace(reSpace.ReplaceAllString(kept, " "))

	// 如果清洗后没有有效内容，就回退到原始 query，避免误杀。
	if kept == "" {
		return q, ""
	}

	// 当前实现普通匹配词和 phrase 都使用清洗后的文本。
	return kept, kept
}

// buildPhraseShould 构建 ES 的短语加权查询。
func buildPhraseShould(phrase string) interface{} {
	// 没有 phrase 时返回 nil，ES 查询里等价于不加短语 should 条件。
	if phrase == "" {
		return nil
	}

	// 返回一个 should 查询数组。
	return []map[string]interface{}{
		// 数组里目前只有一个 match_phrase 条件。
		{
			// match_phrase 要求文本里出现连续短语。
			"match_phrase": map[string]interface{}{
				// 在 text_content 字段上做短语匹配。
				"text_content": map[string]interface{}{
					// query 是核心短语。
					"query": phrase,
					// boost 提高短语完整命中的相关性分数。
					"boost": 3.0,
				},
			},
		},
	}
}
