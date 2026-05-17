package tika

import (
	"RAG-repository/internal/config"
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
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

// knownMimeTypes 是常见文件扩展名到 MIME 类型的硬编码映射，
// 用于补偿 Windows 系统注册表中可能缺失的 MIME 类型条目。
var knownMimeTypes = map[string]string{
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".txt":  "text/plain",
	".md":   "text/plain",
	".html": "text/html",
	".htm":  "text/html",
	".xml":  "application/xml",
	".json": "application/json",
	".csv":  "text/csv",
	".rtf":  "application/rtf",
	".odt":  "application/vnd.oasis.opendocument.text",
}

// detectMimeType 根据文件后缀推断 Content-Type。
// Tika 依赖 Content-Type 来更准确地选择解析器。
// 优先使用硬编码映射，再回退到系统 MIME 注册表，最后使用 octet-stream。
func detectMimeType(fileName string) string {
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext == "" {
		return "application/octet-stream"
	}

	if mimeType, ok := knownMimeTypes[ext]; ok {
		return mimeType
	}

	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		return "application/octet-stream"
	}

	return mimeType
}
