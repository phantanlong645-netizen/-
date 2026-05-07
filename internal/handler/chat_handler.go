// Package handler 放 HTTP 控制器。
package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"RAG-repository/internal/service"
	"RAG-repository/pkg/log"
	"RAG-repository/pkg/token"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// ChatHandler 负责处理 WebSocket 聊天连接。
type ChatHandler struct {
	chatService   service.ChatService
	userService   service.UserService
	jwtManager    *token.JWTManager
	stopToken     string
	stopTokenLock sync.Mutex
	stopFlags     sync.Map
}

// NewChatHandler 创建聊天 Handler。
func NewChatHandler(chatService service.ChatService, userService service.UserService, jwtManager *token.JWTManager) *ChatHandler {
	return &ChatHandler{
		chatService: chatService,
		userService: userService,
		jwtManager:  jwtManager,
	}
}

// GetWebsocketStopToken 生成停止流式输出的命令 token。
// 原项目这里存在内存里，单机可用；多实例部署时应该放 Redis。
func (h *ChatHandler) GetWebsocketStopToken(c *gin.Context) {
	h.stopTokenLock.Lock()
	defer h.stopTokenLock.Unlock()

	h.stopToken = "WSS_STOP_CMD_" + token.GenerateRandomString(16)

	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "success",
		"data": gin.H{
			"cmdToken": h.stopToken,
		},
	})
}

// Handle 处理 WebSocket 连接。
// token 从 URL path 里拿：/chat/:token。
func (h *ChatHandler) Handle(c *gin.Context) {
	tokenString := c.Param("token")

	claims, err := h.jwtManager.VerifyToken(tokenString)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"code":    http.StatusUnauthorized,
			"message": "无效的 token",
			"data":    nil,
		})
		return
	}

	user, err := h.userService.GetProfile(claims.Username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    http.StatusInternalServerError,
			"message": "无法获取用户信息",
			"data":    nil,
		})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Error("WebSocket 升级失败", err)
		return
	}
	defer conn.Close()

	log.Infof("WebSocket 连接已建立，用户: %s", claims.Username)

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Warnf("从 WebSocket 读取消息失败: %v", err)
			break
		}

		var ctrl map[string]interface{}
		if len(message) > 0 && message[0] == '{' {
			if err := json.Unmarshal(message, &ctrl); err == nil {
				if t, ok := ctrl["type"].(string); ok && t == "stop" {
					if tok, ok := ctrl["_internal_cmd_token"].(string); ok && h.isValidStopToken(tok) {
						h.stopFlags.Store(sessionKey(conn), true)
						h.writeStopAck(conn)
						continue
					}
				}
			}
		}

		if string(message) == h.currentStopToken() {
			log.Info("收到停止指令，正在中断流式响应")
			h.stopFlags.Store(sessionKey(conn), true)
			continue
		}

		key := sessionKey(conn)
		h.stopFlags.Delete(key)

		shouldStop := func() bool {
			v, ok := h.stopFlags.Load(key)
			return ok && v.(bool)
		}

		if err := h.chatService.StreamResponse(c.Request.Context(), string(message), user, conn, shouldStop); err != nil {
			log.Errorf("处理流式响应失败: %v", err)

			errResp := map[string]string{"error": "AI服务暂时不可用，请稍后重试"}
			b, _ := json.Marshal(errResp)
			_ = conn.WriteMessage(websocket.TextMessage, b)

			completion := map[string]interface{}{
				"type":      "completion",
				"status":    "finished",
				"message":   "响应已完成",
				"timestamp": time.Now().UnixMilli(),
				"date":      time.Now().Format("2006-01-02T15:04:05"),
			}
			cb, _ := json.Marshal(completion)
			_ = conn.WriteMessage(websocket.TextMessage, cb)
			break
		}
	}
}

func (h *ChatHandler) isValidStopToken(tok string) bool {
	h.stopTokenLock.Lock()
	defer h.stopTokenLock.Unlock()
	return tok == h.stopToken
}

func (h *ChatHandler) currentStopToken() string {
	h.stopTokenLock.Lock()
	defer h.stopTokenLock.Unlock()
	return h.stopToken
}

func (h *ChatHandler) writeStopAck(conn *websocket.Conn) {
	resp := map[string]interface{}{
		"type":      "stop",
		"message":   "响应已停止",
		"timestamp": time.Now().UnixMilli(),
		"date":      time.Now().Format("2006-01-02T15:04:05"),
	}
	b, _ := json.Marshal(resp)
	_ = conn.WriteMessage(websocket.TextMessage, b)
}

func sessionKey(conn *websocket.Conn) string {
	return fmt.Sprintf("%p", conn)
}
