// Package handler 放 HTTP 控制器。
package handler

import (
	"net/http"

	"RAG-repository/internal/service"
	"RAG-repository/pkg/token"

	"github.com/gin-gonic/gin"
)

// ConversationHandler 处理对话历史相关接口。
type ConversationHandler struct {
	service service.ConversationService
}

// NewConversationHandler 创建会话 Handler。
func NewConversationHandler(service service.ConversationService) *ConversationHandler {
	return &ConversationHandler{service: service}
}

// GetConversations 获取当前用户的对话历史。
// userID 来自 AuthMiddleware 写入 Gin Context 的 claims。
func (h *ConversationHandler) GetConversations(c *gin.Context) {
	claims := c.MustGet("claims").(*token.CustomClaims)

	history, err := h.service.GetConversationHistory(c.Request.Context(), claims.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": "Failed to retrieve conversation history",
			"data":    nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "success",
		"data":    history,
	})
}
