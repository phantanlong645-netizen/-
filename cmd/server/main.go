package main

import (
	"RAG-repository/internal/config"
	"RAG-repository/internal/handler"
	"RAG-repository/internal/middleware"
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

	_ = adminService
	_ = conversationService
	_ = chatService

	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}
	router := gin.Default()

	registerRoutes(router, userService, uploadService, documentService, jwtManager)

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
func registerRoutes(
	r *gin.Engine,
	userService service.UserService,
	uploadService service.UploadService,
	documentService service.DocumentService,
	jwtManager *token.JWTManager,
) {
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	r.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{"message": "Welcome to RAG-repository API"})
	})

	apiV1 := r.Group("/api/v1")
	{
		auth := apiV1.Group("/auth")
		{
			auth.POST("/refreshToken", handler.NewAuthHandler(userService).RefreshToken)
		}

		users := apiV1.Group("/users")
		{
			users.POST("/register", handler.NewUserHandler(userService).Register)
			users.POST("/login", handler.NewUserHandler(userService).Login)

			authed := users.Group("/")
			authed.Use(middleware.AuthMiddleware(jwtManager, userService))
			{
				authed.GET("/me", handler.NewUserHandler(userService).GetProfile)
				authed.POST("/logout", handler.NewUserHandler(userService).Logout)
				authed.PUT("/primary-org", handler.NewUserHandler(userService).SetPrimaryOrg)
				authed.GET("/org-tags", handler.NewUserHandler(userService).GetUserOrgTags)
			}
		}

		upload := apiV1.Group("/upload")
		upload.Use(middleware.AuthMiddleware(jwtManager, userService))
		{
			upload.POST("/check", handler.NewUploadHandler(uploadService).CheckFile)
			upload.POST("/chunk", handler.NewUploadHandler(uploadService).UploadChunk)
			upload.POST("/merge", handler.NewUploadHandler(uploadService).MergeChunks)
			upload.GET("/status", handler.NewUploadHandler(uploadService).GetUploadStatus)
			upload.GET("/supported-types", handler.NewUploadHandler(uploadService).GetSupportedFileTypes)
			upload.POST("/fast-upload", handler.NewUploadHandler(uploadService).FastUpload)
		}

		documents := apiV1.Group("/documents")
		documents.Use(middleware.AuthMiddleware(jwtManager, userService))
		{
			documents.GET("/accessible", handler.NewDocumentHandler(documentService, userService).ListAccessibleFiles)
			documents.GET("/uploads", handler.NewDocumentHandler(documentService, userService).ListUploadedFiles)
			documents.DELETE("/:fileMd5", handler.NewDocumentHandler(documentService, userService).DeleteDocument)
			documents.GET("/download", handler.NewDocumentHandler(documentService, userService).GenerateDownloadURL)
			documents.GET("/preview", handler.NewDocumentHandler(documentService, userService).PreviewFile)
		}

	}
}
