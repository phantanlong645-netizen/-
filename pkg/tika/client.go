package tika

import (
	"RAG-repository/internal/config"
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
)

// Client 是 Apache Tika 服务的客户端。
// 它只保存 Tika 服务地址，不直接保存文件状态。
type Client struct {
	serverURL string
}

// NewClient 根据配置创建 Tika 客户端。
// cfg.ServerURL 来自 config.yaml 里的 tika.server_url。
func NewClient(cfg config.TikaConfig) *Client {
	return &Client{serverURL: cfg.ServerURL}
}

// ExtractText 把文件内容发送给 Tika，并返回 Tika 提取出来的纯文本。
// fileReader 是文件流，后面通常来自 MinIO 的 GetObject。
// fileName 用来推断 MIME 类型，例如 .pdf -> application/pdf。
func (c *Client) ExtractText(fileReader io.Reader, fileName string) (string, error) {
	contentType := detectMimeType(fileName)

	req, err := http.NewRequest("PUT", c.serverURL+"/tika", fileReader)
	if err != nil {
		return "", fmt.Errorf("创建 Tika 请求失败: %w", err)
	}

	req.Header.Set("Accept", "text/plain")
	req.Header.Set("Content-Type", contentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("调用 Tika 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Tika 返回错误 [%d]: %s", resp.StatusCode, string(body))
	}

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, resp.Body); err != nil {
		return "", fmt.Errorf("读取 Tika 响应失败: %w", err)
	}

	return buf.String(), nil
}

// detectMimeType 根据文件后缀推断 Content-Type。
// Tika 依赖 Content-Type 来更准确地选择解析器。
func detectMimeType(fileName string) string {
	ext := filepath.Ext(fileName)
	if ext == "" {
		return "application/octet-stream"
	}

	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		return "application/octet-stream"
	}

	return mimeType
}
