// Package middleware 放 Gin 中间件。
package middleware

import (
	"net/http"
	"strings"

	"RAG-repository/internal/service"
	"RAG-repository/pkg/token"

	"github.com/gin-gonic/gin"
)

// AuthMiddleware 做 JWT 登录认证。
// 它必须挂在需要登录的路由前面，比如 /upload、/documents、/search。
// 成功后会往 Gin Context 里放两个值：
// 1. user：数据库里的完整用户对象
// 2. claims：JWT 里解析出来的用户信息
func AuthMiddleware(jwtManager *token.JWTManager, userService service.UserService) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 从请求头里读取 Authorization。
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "请求未包含授权头"})
			return
		}

		// 前端传 token 的标准格式是：Bearer xxxxx。
		const bearerPrefix = "Bearer "
		if !strings.HasPrefix(authHeader, bearerPrefix) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的授权头格式"})
			return
		}

		// 去掉 Bearer 前缀，拿到真正的 JWT 字符串。
		tokenString := strings.TrimPrefix(authHeader, bearerPrefix)

		// 校验 JWT 签名、过期时间，并解析出 claims。
		claims, err := jwtManager.VerifyToken(tokenString)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效或已过期的 token"})
			return
		}

		// 根据 token 里的 username 查数据库。
		// 这一步是为了防止 token 没过期，但用户已经被删除或禁用。
		user, err := userService.GetProfile(claims.Username)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "用户不存在"})
			return
		}

		// 保存完整用户对象，后面的管理员鉴权会用到。
		c.Set("user", user)

		// 保存 claims，普通业务 handler 会用它拿 userID。
		c.Set("claims", claims)

		// 放行，继续进入真正的 handler。
		c.Next()
	}
}
