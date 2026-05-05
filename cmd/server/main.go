package main

import (
	"RAG-repository/internal/config"
	"RAG-repository/internal/pipeline"
	"RAG-repository/internal/repository"
	"RAG-repository/internal/service"
	"RAG-repository/pkg/database"
	"RAG-repository/pkg/embedding"
	"RAG-repository/pkg/es"
	"RAG-repository/pkg/kafka"
	"RAG-repository/pkg/llm"
	"RAG-repository/pkg/log"
	"RAG-repository/pkg/storage"
	"RAG-repository/pkg/tika"
	"RAG-repository/pkg/token"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
)

func main() {
	config.Init("./config/config.yaml")
	cfg := config.Conf

	log.Init(cfg.Log.Level, cfg.Log.Format, cfg.Log.OutputPath)
	defer log.Sync()
	log.Info("log initialized successfully")

	database.InitMySQL(cfg.Database.MySQL.DSN)
	database.InitRedis(cfg.Database.Redis.Addr, cfg.Database.Redis.Password, cfg.Database.Redis.DB)
	storage.InitMinIO(cfg.MinIO)
	if err := es.InitES(cfg.Elasticsearch); err != nil {
		log.Errorf("es 初始化失败 %s", err)
		return
	}
	kafka.InitProducer(cfg.Kafka)
	tikaClient := tika.NewClient(cfg.Tika)
	embeddingClient := embedding.NewClient(cfg.Embedding)
	_ = tikaClient
	_ = embeddingClient
	llmClient := llm.NewClient(cfg.LLM)
	_ = llmClient
	jwtManager := token.NewJWTManager(
		cfg.JWT.Secret,
		cfg.JWT.AccessTokenExpireHours,
		cfg.JWT.RefreshTokenExpireDays,
	)
	_ = jwtManager
	userRepository := repository.NewUserRepository(database.DB)
	orgTagRepo := repository.NewOrgTagRepository(database.DB)
	uploadRepo := repository.NewUploadRepository(database.DB, database.RDB)
	conversationRepo := repository.NewConversationRepository(database.RDB)
	docVectorRepo := repository.NewDocumentVectorRepository(database.DB)
	_ = userRepository
	_ = orgTagRepo
	_ = uploadRepo
	_ = conversationRepo
	_ = docVectorRepo
	userService := service.NewUserService(userRepository, orgTagRepo, jwtManager)
	adminService := service.NewAdminService(orgTagRepo, userRepository, conversationRepo)
	uploadService := service.NewUploadService(uploadRepo, userRepository, cfg.MinIO)
	documentService := service.NewDocumentService(uploadRepo, userRepository, orgTagRepo, cfg.MinIO, tikaClient)
	searchService := service.NewSearchService(embeddingClient, es.ESClient, userService, uploadRepo)
	conversationService := service.NewConversationService(conversationRepo)
	chatService := service.NewChatService(searchService, llmClient, conversationRepo)
	processor := pipeline.NewProcessor(
		tikaClient,
		embeddingClient,
		cfg.Elasticsearch,
		cfg.MinIO,
		cfg.Embedding,
		uploadRepo,
		docVectorRepo,
	)

	go kafka.StartConsumer(cfg.Kafka, processor)

	_ = userService
	_ = adminService
	_ = uploadService
	_ = documentService
	_ = conversationService
	_ = chatService

	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}
	router := gin.Default()

	registerRoutes(router)

	serverAddr := fmt.Sprintf(":%s", cfg.Server.Port)
	log.Infof("Server starting on %s", serverAddr)

	go func() {
		if err := router.Run(serverAddr); err != nil {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("Shutting down server...")
}

func registerRoutes(r *gin.Engine) {
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	r.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{"message": "Welcome to RAG-repository API"})
	})
}
