package storage

import (
	// 引入项目配置包，用来接收 config.yaml 里的 minio 配置。
	"RAG-repository/internal/config"

	// 引入项目日志包，统一使用 zap 封装后的日志能力。
	"RAG-repository/pkg/log"

	// context 用来控制请求生命周期，这里用 Background 创建基础上下文。
	"context"

	// time 用来表示预签名 URL 的有效期。
	"time"

	// MinIO 官方 Go SDK，用来连接和操作对象存储。
	"github.com/minio/minio-go/v7"

	// credentials 用来创建 MinIO 访问凭证。
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinioClient 是全局 MinIO 客户端。
// 后续上传分片、合并文件、生成下载链接都会复用这个客户端。
var MinioClient *minio.Client

// InitMinIO 初始化 MinIO 客户端，并确保配置里的 bucket 已经存在。
func InitMinIO(cfg config.MinIOConfig) {
	// 提前声明 err，方便后面多次复用。
	var err error

	// 创建 MinIO 客户端。
	// cfg.Endpoint 是 MinIO 地址，例如 127.0.0.1:9000。
	MinioClient, err = minio.New(cfg.Endpoint, &minio.Options{
		// 使用 access key 和 secret key 创建静态凭证。
		Creds: credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),

		// Secure 决定是否使用 HTTPS。
		Secure: cfg.UseSSL,
	})

	// 如果客户端创建失败，说明 MinIO 配置或网络连接有问题，直接终止程序。
	if err != nil {
		log.Fatal("failed to initialize MinIO client", err)
	}

	// 记录 MinIO 客户端初始化成功。
	log.Info("MinIO client initialized successfully")

	// 创建一个基础 context，用于后续调用 MinIO SDK。
	ctx := context.Background()

	// 从配置中取出 bucket 名称。
	bucketName := cfg.BucketName

	// 检查 bucket 是否已经存在。
	exists, err := MinioClient.BucketExists(ctx, bucketName)

	// 如果检查 bucket 时出错，说明 MinIO 服务不可用或权限异常。
	if err != nil {
		log.Fatal("failed to check MinIO bucket", err)
	}

	// 如果 bucket 不存在，就主动创建。
	if !exists {
		// 打印 bucket 创建日志。
		log.Infof("bucket '%s' does not exist, creating...", bucketName)

		// 调用 MinIO SDK 创建 bucket。
		if err := MinioClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{}); err != nil {
			log.Fatal("failed to create MinIO bucket", err)
		}

		// 创建成功后记录日志。
		log.Infof("bucket '%s' created successfully", bucketName)

		// bucket 已创建，函数结束。
		return
	}

	// 如果 bucket 已存在，只记录日志，不重复创建。
	log.Infof("bucket '%s' already exists", bucketName)
}

// GetPresignedURL 为指定对象生成一个临时访问 URL。
// 后面文件预览、文件下载会用它。
func GetPresignedURL(bucketName, objectName string, expiry time.Duration) (string, error) {
	// 调用 MinIO SDK 生成 GET 类型的预签名 URL。
	// bucketName 是桶名，objectName 是对象名，expiry 是链接有效期。
	presignedURL, err := MinioClient.PresignedGetObject(
		context.Background(),
		bucketName,
		objectName,
		expiry,
		nil,
	)

	// 如果生成失败，记录错误并把错误返回给调用方。
	if err != nil {
		log.Errorf("Error generating presigned URL: %s", err)
		return "", err
	}

	// SDK 返回的是 url.URL 对象，转成字符串返回。
	return presignedURL.String(), nil
}
