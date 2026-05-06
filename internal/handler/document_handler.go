// Package handler 放 HTTP 控制器。
package handler

import (
	"net/http"

	"RAG-repository/internal/model"
	"RAG-repository/internal/service"
	"RAG-repository/pkg/log"
	"RAG-repository/pkg/token"

	"github.com/gin-gonic/gin"
)

// DocumentHandler 负责处理文档管理相关接口。
type DocumentHandler struct {
	docService  service.DocumentService
	userService service.UserService
}

// NewDocumentHandler 创建文档 Handler。
func NewDocumentHandler(docService service.DocumentService, userService service.UserService) *DocumentHandler {
	return &DocumentHandler{
		docService:  docService,
		userService: userService,
	}
}

// ListAccessibleFiles 获取当前用户可访问的文件列表。
// 可访问 = 自己上传的 + 自己组织标签能访问的 + public 文件。
func (h *DocumentHandler) ListAccessibleFiles(c *gin.Context) {
	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	files, err := h.docService.ListAccessibleFiles(user)
	if err != nil {
		log.Error("ListAccessibleFiles: failed", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取文件列表失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "获取可访问文件列表成功",
		"data":    files,
	})
}

// ListUploadedFiles 获取当前用户自己上传过的文件列表。
func (h *DocumentHandler) ListUploadedFiles(c *gin.Context) {
	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	files, err := h.docService.ListUploadedFiles(user.ID)
	if err != nil {
		log.Error("ListUploadedFiles: failed", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取文件列表失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "获取用户上传文件列表成功",
		"data":    files,
	})
}

// DeleteDocument 删除指定 MD5 的文档。
func (h *DocumentHandler) DeleteDocument(c *gin.Context) {
	fileMD5 := c.Param("fileMd5")
	if fileMD5 == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少文件 MD5"})
		return
	}

	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	if err := h.docService.DeleteDocument(fileMD5, user); err != nil {
		log.Warnf("DeleteDocument: failed for user %s, md5 %s, err: %v", user.Username, fileMD5, err)
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "文档删除成功",
	})
}

// GenerateDownloadURL 生成文件临时下载链接。
func (h *DocumentHandler) GenerateDownloadURL(c *gin.Context) {
	fileName := c.Query("fileName")
	if fileName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少文件名"})
		return
	}

	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	downloadInfo, err := h.docService.GenerateDownloadURL(fileName, user)
	if err != nil {
		log.Warnf("GenerateDownloadURL: failed for user %s, file %s, err: %v", user.Username, fileName, err)
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "文件下载链接生成成功",
		"data":    downloadInfo,
	})
}

// PreviewFile 获取文件预览内容。
func (h *DocumentHandler) PreviewFile(c *gin.Context) {
	fileName := c.Query("fileName")
	if fileName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少文件名"})
		return
	}

	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	previewInfo, err := h.docService.GetFilePreviewContent(fileName, user)
	if err != nil {
		log.Warnf("PreviewFile: failed for user %s, file %s, err: %v", user.Username, fileName, err)
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "文件预览内容获取成功",
		"data":    previewInfo,
	})
}

// getUserFromContext 从 Gin 上下文里拿 claims，再查数据库拿完整用户。
// 原项目没有直接用 c.Get("user")，而是通过 claims.Username 再查一次用户。
func (h *DocumentHandler) getUserFromContext(c *gin.Context) (*model.User, error) {
	claimsValue, _ := c.Get("claims")
	claims := claimsValue.(*token.CustomClaims)

	return h.userService.GetProfile(claims.Username)
}
