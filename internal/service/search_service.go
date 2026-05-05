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

type SearchService interface {
	HybridSearch(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error)
}

type searchService struct {
	embeddingClient embedding.Client
	esClient        *elasticsearch.Client
	userService     UserService
	uploadRepo      repository.UploadRepository
}

func NewSearchService(
	embeddingClient embedding.Client,
	esClient *elasticsearch.Client,
	userService UserService,
	uploadRepo repository.UploadRepository,
) SearchService {
	return &searchService{
		embeddingClient: embeddingClient,
		esClient:        esClient,
		userService:     userService,
		uploadRepo:      uploadRepo,
	}
}

func (s *searchService) HybridSearch(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error) {
	log.Infof("[SearchService] 开始执行混合搜索, query: '%s', topK: %d, user: %s", query, topK, user.Username)

	userEffectiveTags, err := s.userService.GetUserEffectiveOrgTags(user)
	if err != nil {
		log.Errorf("[SearchService] 获取用户有效组织标签失败: %v", err)
		userEffectiveTags = []string{}
	}
	log.Infof("[SearchService] 获取到 %d 个有效组织标签: %v", len(userEffectiveTags), userEffectiveTags)

	normalized, phrase := normalizeQuery(query)
	if normalized != query {
		log.Infof("[SearchService] 规范化查询: '%s' -> '%s' (phrase='%s')", query, normalized, phrase)
	}

	queryVector, err := s.embeddingClient.CreateEmbedding(ctx, query)
	if err != nil {
		log.Errorf("[SearchService] 向量化查询失败: %v", err)
		return nil, fmt.Errorf("failed to create query embedding: %w", err)
	}
	log.Infof("[SearchService] 向量化查询成功, 向量维度: %d", len(queryVector))
	var buf bytes.Buffer

	esQuery := map[string]interface{}{
		"knn": map[string]interface{}{
			"field":          "vector",
			"query_vector":   queryVector,
			"k":              topK * 30,
			"num_candidates": topK * 30,
		},
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				"must": map[string]interface{}{
					"match": map[string]interface{}{
						"text_content": normalized,
					},
				},
				"filter": map[string]interface{}{
					"bool": map[string]interface{}{
						"should": []map[string]interface{}{
							{"term": map[string]interface{}{"user_id": user.ID}},
							{"term": map[string]interface{}{"is_public": true}},
							{"terms": map[string]interface{}{"org_tag": userEffectiveTags}},
						},
						"minimum_should_match": 1,
					},
				},
				"should": buildPhraseShould(phrase),
			},
		},
		"rescore": map[string]interface{}{
			"window_size": topK * 30,
			"query": map[string]interface{}{
				"rescore_query": map[string]interface{}{
					"match": map[string]interface{}{
						"text_content": map[string]interface{}{
							"query":    normalized,
							"operator": "and",
						},
					},
				},
				"query_weight":         0.2,
				"rescore_query_weight": 1.0,
			},
		},
		"size": topK,
	}

	if err := json.NewEncoder(&buf).Encode(esQuery); err != nil {
		log.Errorf("[SearchService] 序列化 Elasticsearch 查询失败: %v", err)
		return nil, fmt.Errorf("failed to encode es query: %w", err)
	}

	log.Infof("[SearchService] 构建的 Elasticsearch 查询语句: %s", buf.String())

	res, err := s.esClient.Search(
		s.esClient.Search.WithContext(ctx),
		s.esClient.Search.WithIndex("knowledge_base"),
		s.esClient.Search.WithBody(&buf),
		s.esClient.Search.WithTrackTotalHits(true),
	)
	if err != nil {
		log.Errorf("[SearchService] 向 Elasticsearch 发送搜索请求失败: %v", err)
		return nil, fmt.Errorf("elasticsearch search failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		bodyBytes, _ := io.ReadAll(res.Body)
		log.Errorf("[SearchService] Elasticsearch 返回错误, status: %s, body: %s", res.Status(), string(bodyBytes))
		return nil, fmt.Errorf("elasticsearch returned an error: %s", res.String())
	}

	log.Info("[SearchService] 成功从 Elasticsearch 获取响应")
	var esResponse struct {
		Hits struct {
			Hits []struct {
				Source model.EsDocument `json:"_source"`
				Score  float64          `json:"_score"`
			} `json:"hits"`
		} `json:"hits"`
	}

	if err := json.NewDecoder(res.Body).Decode(&esResponse); err != nil {
		log.Errorf("[SearchService] 解析 Elasticsearch 响应失败: %v", err)
		return nil, fmt.Errorf("failed to decode es response: %w", err)
	}

	if len(esResponse.Hits.Hits) == 0 {
		log.Infof("[SearchService] Elasticsearch 返回 0 条命中结果")
		if phrase != "" && phrase != query {
			log.Infof("[SearchService] 使用核心短语重试查询: '%s'", phrase)

			var retryBuf bytes.Buffer
			retryQuery := esQuery

			((retryQuery["query"].(map[string]interface{}))["bool"].(map[string]interface{}))["must"] = map[string]interface{}{
				"match": map[string]interface{}{
					"text_content": phrase,
				},
			}

			((retryQuery["rescore"].(map[string]interface{}))["query"].(map[string]interface{}))["rescore_query"] = map[string]interface{}{
				"match": map[string]interface{}{
					"text_content": map[string]interface{}{
						"query":    phrase,
						"operator": "and",
					},
				},
			}

			if err := json.NewEncoder(&retryBuf).Encode(retryQuery); err == nil {
				res2, err2 := s.esClient.Search(
					s.esClient.Search.WithContext(ctx),
					s.esClient.Search.WithIndex("knowledge_base"),
					s.esClient.Search.WithBody(&retryBuf),
					s.esClient.Search.WithTrackTotalHits(true),
				)

				if err2 == nil && !res2.IsError() {
					defer res2.Body.Close()

					if err := json.NewDecoder(res2.Body).Decode(&esResponse); err == nil {
						log.Infof("[SearchService] 重试后命中 %d 条", len(esResponse.Hits.Hits))
					}
				}
			}
		}

		if len(esResponse.Hits.Hits) == 0 {
			return []model.SearchResponseDTO{}, nil
		}
	}

	fileMD5s := make([]string, 0, len(esResponse.Hits.Hits))
	for _, hit := range esResponse.Hits.Hits {
		fileMD5s = append(fileMD5s, hit.Source.FileMD5)
	}

	uniqueMD5s := make(map[string]struct{})
	for _, md5 := range fileMD5s {
		uniqueMD5s[md5] = struct{}{}
	}

	md5List := make([]string, 0, len(uniqueMD5s))
	for md5 := range uniqueMD5s {
		md5List = append(md5List, md5)
	}

	fileInfos, err := s.uploadRepo.FindBatchByMD5s(md5List)
	if err != nil {
		log.Errorf("[SearchService] 批量查询文件信息失败: %v", err)
		return nil, fmt.Errorf("批量查询文件信息失败: %w", err)
	}

	fileNameMap := make(map[string]string)
	for _, info := range fileInfos {
		fileNameMap[info.FileMD5] = info.FileName
	}

	results := make([]model.SearchResponseDTO, 0, len(esResponse.Hits.Hits))
	for _, hit := range esResponse.Hits.Hits {
		fileName := fileNameMap[hit.Source.FileMD5]
		if fileName == "" {
			log.Warnf("[SearchService] 未找到 FileMD5 '%s' 对应的文件名，将使用 '未知文件'", hit.Source.FileMD5)
			fileName = "未知文件"
		}

		dto := model.SearchResponseDTO{
			FileMD5:     hit.Source.FileMD5,
			FileName:    fileName,
			ChunkID:     hit.Source.ChunkID,
			TextContent: hit.Source.TextContent,
			Score:       hit.Score,
			UserID:      strconv.FormatUint(uint64(hit.Source.UserID), 10),
			OrgTag:      hit.Source.OrgTag,
			IsPublic:    hit.Source.IsPublic,
		}

		results = append(results, dto)
	}

	log.Infof("[SearchService] 组装最终响应成功，返回 %d 条结果", len(results))
	return results, nil

}
func normalizeQuery(q string) (string, string) {
	if q == "" {
		return q, ""
	}

	lower := strings.ToLower(q)

	stopPhrases := []string{
		"是谁", "是什么", "是啥", "请问", "怎么", "如何",
		"告诉我", "严格", "按照", "不要补充", "的区别",
		"区别", "吗", "呢", "？", "?",
	}

	for _, sp := range stopPhrases {
		lower = strings.ReplaceAll(lower, sp, " ")
	}

	reKeep := regexp.MustCompile(`[^\p{Han}a-z0-9\s]+`)
	kept := reKeep.ReplaceAllString(lower, " ")

	reSpace := regexp.MustCompile(`\s+`)
	kept = strings.TrimSpace(reSpace.ReplaceAllString(kept, " "))

	if kept == "" {
		return q, ""
	}

	return kept, kept
}

func buildPhraseShould(phrase string) interface{} {
	if phrase == "" {
		return nil
	}

	return []map[string]interface{}{
		{
			"match_phrase": map[string]interface{}{
				"text_content": map[string]interface{}{
					"query": phrase,
					"boost": 3.0,
				},
			},
		},
	}
}
