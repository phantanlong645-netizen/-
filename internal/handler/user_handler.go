// Package handler 放 HTTP 控制器。
package handler

import (
	"net/http"
	"strings"
	"time"

	"RAG-repository/internal/model"
	"RAG-repository/internal/service"
	"RAG-repository/pkg/log"

	"github.com/gin-gonic/gin"
)

// UserHandler 负责处理用户注册、登录、个人信息、退出登录等接口。
type UserHandler struct {
	userService service.UserService
}

// NewUserHandler 创建用户 Handler。
func NewUserHandler(userService service.UserService) *UserHandler {
	return &UserHandler{userService: userService}
}

// RegisterRequest 是注册接口的请求体。
type RegisterRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// Register 处理用户注册。
func (h *UserHandler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Warnf("Register: invalid request payload, error: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    http.StatusBadRequest,
			"message": "无效的请求参数：用户名和密码不能为空",
		})
		return
	}

	user, err := h.userService.Register(req.Username, req.Password)
	if err != nil {
		log.Warnf("Register: user registration failed for '%s', error: %v", req.Username, err)
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	log.Infof("User '%s' registered successfully", user.Username)
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "User registered successfully",
	})
}

// LoginRequest 是登录接口的请求体。
type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// Login 处理用户登录。
func (h *UserHandler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Warnf("Login: invalid request payload, error: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    http.StatusBadRequest,
			"message": "无效的请求参数：用户名和密码不能为空",
		})
		return
	}

	accessToken, refreshToken, err := h.userService.Login(req.Username, req.Password)
	if err != nil {
		log.Warnf("Login: user authentication failed for '%s', error: %v", req.Username, err)
		c.JSON(http.StatusUnauthorized, gin.H{
			"code":    http.StatusUnauthorized,
			"message": "无效的用户名或密码",
		})
		return
	}

	log.Infof("User '%s' logged in successfully", req.Username)
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "Login successful",
		"data": gin.H{
			"token":        accessToken,
			"refreshToken": refreshToken,
		},
	})
}

// ProfileResponse 是用户信息响应结构，原项目定义了这个结构，但实际 GetProfile 直接返回 user。
type ProfileResponse struct {
	ID         uint      `json:"id"`
	Username   string    `json:"username"`
	Role       string    `json:"role"`
	OrgTags    []string  `json:"orgTags"`
	PrimaryOrg string    `json:"primaryOrg"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// GetProfile 获取当前登录用户信息。
func (h *UserHandler) GetProfile(c *gin.Context) {
	user, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"data":    user,
		"message": "success",
	})
}

// Logout 处理退出登录。
func (h *UserHandler) Logout(c *gin.Context) {
	authHeader := c.GetHeader("Authorization")
	tokenString := strings.TrimPrefix(authHeader, "Bearer ")

	if err := h.userService.Logout(tokenString); err != nil {
		log.Error("Logout: failed to logout", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": "登出失败",
		})
		return
	}

	userValue, _ := c.Get("user")
	user := userValue.(*model.User)

	log.Infof("User '%s' logged out successfully", user.Username)
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "登出成功",
	})
}

// SetPrimaryOrgRequest 是设置主组织的请求体。
type SetPrimaryOrgRequest struct {
	PrimaryOrg string `json:"primaryOrg" binding:"required"`
}

// SetPrimaryOrg 设置当前用户的主组织。
func (h *UserHandler) SetPrimaryOrg(c *gin.Context) {
	var req SetPrimaryOrgRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Warnf("SetPrimaryOrg: invalid request payload, error: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    http.StatusBadRequest,
			"message": "无效的请求参数",
		})
		return
	}

	userValue, ok := c.Get("user")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"code":    http.StatusUnauthorized,
			"message": "未认证用户或无法获取用户信息",
		})
		return
	}

	user, ok := userValue.(*model.User)
	if !ok || user == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": "用户数据类型错误",
		})
		return
	}

	if err := h.userService.SetUserPrimaryOrg(user.Username, req.PrimaryOrg); err != nil {
		log.Warnf("SetPrimaryOrg: failed for user '%s', error: %v", user.Username, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": err.Error(),
		})
		return
	}

	log.Infof("User '%s' set primary organization to '%s'", user.Username, req.PrimaryOrg)
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "主组织更新成功",
	})
}

// GetUserOrgTags 获取当前用户的组织标签。
func (h *UserHandler) GetUserOrgTags(c *gin.Context) {
	userValue, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	user, ok := userValue.(*model.User)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "用户数据类型错误"})
		return
	}

	tags, err := h.userService.GetUserOrgTags(user.Username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"data":    tags,
		"message": "success",
	})
}
