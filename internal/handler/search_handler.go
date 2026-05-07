// Package handler 放 HTTP 控制器。
package handler

import (
	"net/http"
	"strconv"

	"RAG-repository/internal/model"
	"RAG-repository/internal/service"
	"RAG-repository/pkg/log"

	"github.com/gin-gonic/gin"
)

// SearchHandler 负责搜索相关接口。
type SearchHandler struct {
	searchService service.SearchService
}

// NewSearchHandler 创建搜索 Handler。
func NewSearchHandler(searchService service.SearchService) *SearchHandler {
	return &SearchHandler{
		searchService: searchService,
	}
}

// HybridSearch 处理混合搜索请求。
// 混合搜索 = 向量相似度搜索 + ES 文本匹配 + 权限过滤。
func (h *SearchHandler) HybridSearch(c *gin.Context) {
	query := c.Query("query")
	log.Infof("[SearchHandler] 收到混合搜索请求, query: %s", query)

	if query == "" {
		log.Warnf("[SearchHandler] 搜索失败: query 参数为空")
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的查询参数"})
		return
	}

	topKStr := c.DefaultQuery("topK", "10")
	topK, err := strconv.Atoi(topKStr)
	if err != nil || topK <= 0 {
		topK = 10
	}

	userValue, exists := c.Get("user")
	if !exists {
		log.Errorf("[SearchHandler] 无法从 Gin 上下文获取用户信息")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	user, ok := userValue.(*model.User)
	if !ok || user == nil {
		log.Errorf("[SearchHandler] 用户数据类型错误")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "用户数据类型错误"})
		return
	}

	results, err := h.searchService.HybridSearch(c.Request.Context(), query, topK, user)
	if err != nil {
		log.Errorf("[SearchHandler] 混合搜索失败, error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "搜索失败"})
		return
	}

	log.Infof("[SearchHandler] 混合搜索成功, query: '%s', 返回 %d 条结果", query, len(results))
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"data":    results,
		"message": "success",
	})
}
