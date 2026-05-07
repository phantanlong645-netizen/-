// Package middleware 放 Gin 中间件。
package middleware

import (
	"net/http"

	"RAG-repository/internal/model"

	"github.com/gin-gonic/gin"
)

// AdminAuthMiddleware 检查当前用户是否是管理员。
// 注意：它必须放在 AuthMiddleware 后面。
// 因为 user 是 AuthMiddleware 通过 c.Set("user", user) 放进去的。
func AdminAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		user, exists := c.Get("user")
		if !exists {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
			return
		}

		currentUser, ok := user.(*model.User)
		if !ok || currentUser == nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "用户数据类型错误"})
			return
		}

		if currentUser.Role != "ADMIN" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "权限不足，需要管理员权限"})
			return
		}

		c.Next()
	}
}
