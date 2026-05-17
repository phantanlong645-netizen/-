package main

import (
	"RAG-repository/internal/config"
	"RAG-repository/internal/handler"
	"RAG-repository/internal/middleware"
	"RAG-repository/internal/model"
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
	if err := database.DB.AutoMigrate(&model.FileUpload{}, &model.DocumentVector{}, &model.ResearchSession{}, &model.ResearchCandidate{}); err != nil {
		log.Errorf("database table migration failed: %v", err)
		return
	}
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
	researchRepo := repository.NewResearchRepository(database.DB)
	_ = userRepository
	_ = orgTagRepo
	_ = uploadRepo
	_ = conversationRepo
	_ = docVectorRepo
	_ = researchRepo
	userService := service.NewUserService(userRepository, orgTagRepo, jwtManager)
	adminService := service.NewAdminService(orgTagRepo, userRepository, conversationRepo)
	uploadService := service.NewUploadService(uploadRepo, userRepository, orgTagRepo, cfg.MinIO)
	documentService := service.NewDocumentService(uploadRepo, userRepository, userService, orgTagRepo, docVectorRepo, cfg.MinIO, cfg.Elasticsearch, tikaClient)
	searchService := service.NewSearchService(embeddingClient, es.ESClient, userService, uploadRepo)
	conversationService := service.NewConversationService(conversationRepo)
	chatService := service.NewChatService(searchService, llmClient, conversationRepo)
	researchAgentService := service.NewResearchAgentService(researchRepo, uploadRepo, orgTagRepo, cfg.MinIO, cfg.ResearchAgent, llmClient)
	processor := pipeline.NewProcessor(
		tikaClient,
		embeddingClient,
		cfg.Elasticsearch,
		cfg.MinIO,
		cfg.Embedding,
		uploadRepo,
		docVectorRepo,
	)

	consumerCount := cfg.Kafka.ConsumerCount
	if consumerCount <= 0 {
		consumerCount = 1
	}
	for i := 0; i < consumerCount; i++ {
		go kafka.StartConsumer(cfg.Kafka, processor)
	}

	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}
	router := gin.New()
	router.Use(middleware.RequestLogger(), gin.Recovery())

	registerRoutes(router, userService, adminService, uploadService, documentService, searchService, conversationService, chatService, researchAgentService, jwtManager)

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
	adminService service.AdminService,
	uploadService service.UploadService,
	documentService service.DocumentService,
	searchService service.SearchService,
	conversationService service.ConversationService,
	chatService service.ChatService,
	researchAgentService service.ResearchAgentService,
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
			documents.POST("/:fileMd5/vectorization/retry", handler.NewDocumentHandler(documentService, userService).RetryVectorization)
			documents.GET("/download", handler.NewDocumentHandler(documentService, userService).GenerateDownloadURL)
			documents.GET("/preview", handler.NewDocumentHandler(documentService, userService).PreviewFile)
		}

		search := apiV1.Group("/search")
		search.Use(middleware.AuthMiddleware(jwtManager, userService))
		{
			search.GET("/hybrid", handler.NewSearchHandler(searchService).HybridSearch)
		}

		researchAgent := apiV1.Group("/research-agent")
		researchAgent.Use(middleware.AuthMiddleware(jwtManager, userService))
		{
			researchAgentHandler := handler.NewResearchAgentHandler(researchAgentService, userService)
			researchAgent.POST("/sessions", researchAgentHandler.RunSearch)
			researchAgent.GET("/sessions/:sessionId/candidates", researchAgentHandler.ListCandidates)
			researchAgent.POST("/candidates/:candidateId/import", researchAgentHandler.ImportCandidate)
		}

		conversation := apiV1.Group("/users/conversation")
		conversation.Use(middleware.AuthMiddleware(jwtManager, userService))
		{
			conversation.GET("", handler.NewConversationHandler(conversationService).GetConversations)
		}

		chatGroup := apiV1.Group("/chat")
		{
			chatGroup.GET("/websocket-token", handler.NewChatHandler(chatService, userService, jwtManager).GetWebsocketStopToken)
		}

		r.GET("/chat/:token", handler.NewChatHandler(chatService, userService, jwtManager).Handle)

		admin := apiV1.Group("/admin")
		admin.Use(middleware.AuthMiddleware(jwtManager, userService), middleware.AdminAuthMiddleware())
		{
			admin.GET("/users/list", handler.NewAdminHandler(adminService, userService).ListUsers)
			admin.PUT("/users/:userId/org-tags", handler.NewAdminHandler(adminService, userService).AssignOrgTagsToUser)
			admin.GET("/conversation", handler.NewAdminHandler(adminService, userService).GetAllConversations)

			orgTags := admin.Group("/org-tags")
			{
				orgTags.POST("", handler.NewAdminHandler(adminService, userService).CreateOrganizationTag)
				orgTags.GET("", handler.NewAdminHandler(adminService, userService).ListOrganizationTags)
				orgTags.GET("/tree", handler.NewAdminHandler(adminService, userService).GetOrganizationTagTree)
				orgTags.PUT("/:id", handler.NewAdminHandler(adminService, userService).UpdateOrganizationTag)
				orgTags.DELETE("/:id", handler.NewAdminHandler(adminService, userService).DeleteOrganizationTag)
			}
		}

	}
}
