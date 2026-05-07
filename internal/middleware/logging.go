// Package middleware 放 Gin 中间件。
package middleware

import (
	"bytes"
	"io"
	"time"

	"RAG-repository/pkg/log"

	"github.com/gin-gonic/gin"
)

// bodyLogWriter 用来捕获响应体。
// Gin 默认只会把响应写给客户端，这里包一层，把响应内容也复制到 buffer 里。
type bodyLogWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

// Write 拦截响应写入。
// 一边写到 body buffer，方便记录日志；一边继续写给真正的客户端。
func (w bodyLogWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

// RequestLogger 记录请求和响应日志。
// 原项目会记录：状态码、耗时、客户端 IP、方法、路径、请求体、响应体。
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := time.Now()

		var requestBody []byte
		if c.Request.Body != nil {
			requestBody, _ = io.ReadAll(c.Request.Body)
		}

		// 请求体被读过一次后，后面的 handler 就读不到了。
		// 所以这里必须重新塞回去。
		c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))

		blw := &bodyLogWriter{
			body:           bytes.NewBufferString(""),
			ResponseWriter: c.Writer,
		}
		c.Writer = blw

		c.Next()

		log.Infow("HTTP Request Log",
			"statusCode", c.Writer.Status(),
			"latency", time.Since(startTime).String(),
			"clientIP", c.ClientIP(),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"requestBody", string(requestBody),
			"responseBody", blw.body.String(),
		)
	}
}
