// Package handler 放 HTTP 控制器。
package handler

import (
	"net/http"

	"RAG-repository/internal/service"
	"RAG-repository/pkg/log"

	"github.com/gin-gonic/gin"
)

// AuthHandler 负责认证相关接口，比如刷新 token。
type AuthHandler struct {
	userService service.UserService
}

// NewAuthHandler 创建认证 Handler。
func NewAuthHandler(userService service.UserService) *AuthHandler {
	return &AuthHandler{userService: userService}
}

// RefreshTokenRequest 是刷新 token 的请求体。
type RefreshTokenRequest struct {
	RefreshToken string `json:"refreshToken" binding:"required"`
}

// RefreshToken 使用 refresh token 换新的 access token 和 refresh token。
func (h *AuthHandler) RefreshToken(c *gin.Context) {
	var req RefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Warnf("RefreshToken: invalid request payload, error: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的请求参数：refreshToken 不能为空"})
		return
	}

	newAccessToken, newRefreshToken, err := h.userService.RefreshToken(req.RefreshToken)
	if err != nil {
		log.Warnf("RefreshToken: failed to refresh token, error: %v", err)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "无效的 refresh token"})
		return
	}

	log.Info("Token refreshed successfully")
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "Token refreshed successfully",
		"data": gin.H{
			"token":        newAccessToken,
			"refreshToken": newRefreshToken,
		},
	})
}
