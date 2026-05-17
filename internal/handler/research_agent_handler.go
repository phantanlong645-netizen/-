package handler

import (
	"net/http"
	"strconv"

	"RAG-repository/internal/model"
	"RAG-repository/internal/service"
	"RAG-repository/pkg/log"
	"RAG-repository/pkg/token"

	"github.com/gin-gonic/gin"
)

// ResearchAgentHandler 负责处理外部学术检索和导入知识库相关接口
// 此类是HTTP请求的处理入口，接收Gin框架的HTTP请求，调用Service层完成业务逻辑
type ResearchAgentHandler struct {
	researchAgentService service.ResearchAgentService // 学术检索Agent服务实例
	userService          service.UserService          // 用户服务实例，用于获取当前登录用户信息
}

// NewResearchAgentHandler 创建ResearchAgentHandler实例
// 通过依赖注入接收Service层实例，实现层间解耦
func NewResearchAgentHandler(researchAgentService service.ResearchAgentService, userService service.UserService) *ResearchAgentHandler {
	return &ResearchAgentHandler{
		researchAgentService: researchAgentService,
		userService:          userService,
	}
}

// RunSearch 处理学术论文检索请求
// 根据用户问题调用Agent检索外部论文资源，返回候选论文列表和会话信息
func (h *ResearchAgentHandler) RunSearch(c *gin.Context) {
	var req service.ResearchSearchRequest
	// 解析请求JSON body，如果格式错误返回400
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的请求参数"})
		return
	}

	// 从请求上下文获取当前登录用户信息
	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	// 调用Service层的RunSearch方法执行学术检索
	result, err := h.researchAgentService.RunSearch(c.Request.Context(), user, req)
	if err != nil {
		// 记录检索失败日志
		log.Warnf("RunResearchSearch: failed for user %s, err: %v", user.Username, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 检索成功，返回200状态码和结果数据
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "Agent 检索完成",
		"data":    result,
	})
}

// ListCandidates 处理查询某次检索会话下的候选结果请求
// 根据会话ID查询该次检索产生的所有候选论文列表
func (h *ResearchAgentHandler) ListCandidates(c *gin.Context) {
	// 从URL路径参数获取会话ID
	sessionID, err := strconv.ParseUint(c.Param("sessionId"), 10, 64)
	if err != nil || sessionID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的会话 ID"})
		return
	}

	// 从请求上下文获取当前登录用户信息
	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	// 调用Service层方法查询候选论文列表
	candidates, err := h.researchAgentService.ListCandidates(uint(sessionID), user)
	if err != nil {
		log.Warnf("ListResearchCandidates: failed for user %s, sessionID %d, err: %v", user.Username, sessionID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 返回查询结果
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "候选结果获取成功",
		"data":    candidates,
	})
}

// ImportCandidate 处理将外部检索结果导入用户知识库的请求
// 下载论文内容（PDF或Markdown），上传到存储，并触发向量化和文件处理流程
func (h *ResearchAgentHandler) ImportCandidate(c *gin.Context) {
	// 从URL路径参数获取候选结果ID
	candidateID, err := strconv.ParseUint(c.Param("candidateId"), 10, 64)
	if err != nil || candidateID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的候选结果 ID"})
		return
	}

	// 解析请求体中的导入参数（组织标签、是否公开）
	var req service.ResearchImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的请求参数"})
		return
	}

	// 从请求上下文获取当前登录用户信息
	user, err := h.getUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法获取用户信息"})
		return
	}

	// 调用Service层方法执行导入操作
	file, err := h.researchAgentService.ImportCandidate(c.Request.Context(), uint(candidateID), user, req)
	if err != nil {
		log.Warnf("ImportResearchCandidate: failed for user %s, candidateID %d, err: %v", user.Username, candidateID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 导入成功，返回上传记录信息
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "已导入知识库并提交后台处理",
		"data":    file,
	})
}

// getUserFromContext 从Gin上下文中提取JWT token中的用户信息
// 从token claims获取用户名，再通过userService查询完整的用户信息
func (h *ResearchAgentHandler) getUserFromContext(c *gin.Context) (*model.User, error) {
	// 从Gin context中获取之前中间件设置的claims
	claimsValue, _ := c.Get("claims")
	claims := claimsValue.(*token.CustomClaims)

	// 根据用户名获取完整的用户信息
	return h.userService.GetProfile(claims.Username)
}
