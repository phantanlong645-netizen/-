// Package handler 放 HTTP 控制器。
package handler

import (
	"net/http"
	"strconv"

	"RAG-repository/internal/service"
	"RAG-repository/pkg/log"
	"RAG-repository/pkg/token"

	"github.com/gin-gonic/gin"
)

// calculateProgress 根据已上传分片数和总分片数计算上传进度。
func calculateProgress(uploadedChunks []int, totalChunks int) float64 {
	if totalChunks == 0 {
		return 0
	}
	return float64(len(uploadedChunks)) / float64(totalChunks) * 100
}

// UploadHandler 负责处理上传相关接口。
type UploadHandler struct {
	uploadService service.UploadService
}

// NewUploadHandler 创建上传 Handler。
func NewUploadHandler(uploadService service.UploadService) *UploadHandler {
	return &UploadHandler{uploadService: uploadService}
}

type CheckFileRequest struct {
	MD5 string `json:"md5" binding:"required"`
}

// CheckFile 检查文件是否已上传，支持秒传和断点续传。
func (h *UploadHandler) CheckFile(c *gin.Context) {
	var req CheckFileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的请求参数"})
		return
	}

	claimsValue, _ := c.Get("claims")
	userClaims := claimsValue.(*token.CustomClaims)

	completed, uploadedChunks, err := h.uploadService.CheckFile(c.Request.Context(), req.MD5, userClaims.UserID)
	if err != nil {
		log.Error("CheckFile: failed to check file", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"completed":      completed,
		"uploadedChunks": uploadedChunks,
	})
}

// UploadChunk 上传单个文件分片。
func (h *UploadHandler) UploadChunk(c *gin.Context) {
	fileMD5 := c.PostForm("fileMd5")
	fileName := c.PostForm("fileName")
	totalSizeStr := c.PostForm("totalSize")
	chunkIndexStr := c.PostForm("chunkIndex")
	orgTag := c.PostForm("orgTag")
	isPublicStr := c.PostForm("isPublic")

	if fileMD5 == "" || fileName == "" || totalSizeStr == "" || chunkIndexStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少必要参数"})
		return
	}

	totalSize, err := strconv.ParseInt(totalSizeStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的文件大小"})
		return
	}

	chunkIndex, err := strconv.Atoi(chunkIndexStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的分片索引"})
		return
	}

	isPublic, _ := strconv.ParseBool(isPublicStr)

	file, _, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未能获取上传分片"})
		return
	}
	defer file.Close()

	claimsValue, _ := c.Get("claims")
	userClaims := claimsValue.(*token.CustomClaims)

	uploadedChunks, totalChunks, err := h.uploadService.UploadChunk(
		c.Request.Context(),
		fileMD5,
		fileName,
		totalSize,
		chunkIndex,
		file,
		userClaims.UserID,
		orgTag,
		isPublic,
	)
	if err != nil {
		log.Error("UploadChunk: failed to upload chunk", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": "分片上传失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "分片上传成功",
		"data": gin.H{
			"uploaded": uploadedChunks,
			"progress": calculateProgress(uploadedChunks, totalChunks),
		},
	})
}

type MergeChunksRequest struct {
	MD5      string `json:"fileMd5" binding:"required"`
	FileName string `json:"fileName" binding:"required"`
}

// MergeChunks 合并所有分片，并触发 Kafka 后台处理。
func (h *UploadHandler) MergeChunks(c *gin.Context) {
	var req MergeChunksRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的请求参数"})
		return
	}

	claimsValue, _ := c.Get("claims")
	userClaims := claimsValue.(*token.CustomClaims)

	objectURL, err := h.uploadService.MergeChunks(c.Request.Context(), req.MD5, req.FileName, userClaims.UserID)
	if err != nil {
		log.Error("MergeChunks: failed to merge chunks", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "文件合并失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "文件合并成功，任务已发送到 Kafka",
		"data":    gin.H{"object_url": objectURL},
	})
}

// GetUploadStatus 查询文件上传进度。
func (h *UploadHandler) GetUploadStatus(c *gin.Context) {
	fileMD5 := c.Query("file_md5")
	if fileMD5 == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 file_md5 参数"})
		return
	}

	claimsValue, _ := c.Get("claims")
	userClaims := claimsValue.(*token.CustomClaims)

	fileName, fileType, uploadedChunks, totalChunks, err := h.uploadService.GetUploadStatus(c.Request.Context(), fileMD5, userClaims.UserID)
	if err != nil {
		log.Error("GetUploadStatus: failed", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取上传状态失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "获取上传状态成功",
		"data": gin.H{
			"fileName":    fileName,
			"fileType":    fileType,
			"uploaded":    uploadedChunks,
			"progress":    calculateProgress(uploadedChunks, totalChunks),
			"totalChunks": totalChunks,
		},
	})
}

// GetSupportedFileTypes 返回系统支持的文件类型。
func (h *UploadHandler) GetSupportedFileTypes(c *gin.Context) {
	types, err := h.uploadService.GetSupportedFileTypes()
	if err != nil {
		log.Error("GetSupportedFileTypes: failed", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取支持文件类型失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "获取支持文件类型成功",
		"data":    types,
	})
}

// FastUpload 单独做秒传检查。
func (h *UploadHandler) FastUpload(c *gin.Context) {
	var req struct {
		MD5 string `json:"md5" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的请求参数"})
		return
	}

	claims := c.MustGet("claims").(*token.CustomClaims)

	uploaded, err := h.uploadService.FastUpload(c.Request.Context(), req.MD5, claims.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "秒传检查失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"uploaded": uploaded})
}
