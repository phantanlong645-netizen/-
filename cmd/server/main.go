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
	"RAG-repository/pkg/lightrag"
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
	// 初始化对话服务（用于RAG对话）
	conversationService := service.NewConversationService(conversationRepo)
	chatService := service.NewChatService(searchService, llmClient, conversationRepo)
	// 初始化学术检索Agent服务
	// 依赖注入：研究仓库、组织标签仓库、Agent配置、LLM客户端、文件导入器
	researchAgentService := service.NewResearchAgentService(researchRepo, orgTagRepo, cfg.ResearchAgent, llmClient, uploadService)
	processor := pipeline.NewProcessor(
		tikaClient,
		embeddingClient,
		cfg.Elasticsearch,
		cfg.MinIO,
		cfg.Embedding,
		uploadRepo,
		docVectorRepo,
	)

	// 如果开启 LightRAG，创建客户端并挂载到 processor（chatService 在 NewChatService 内部自动初始化）。
	if cfg.LightRAG.Enable {
		lrClient := lightrag.NewClient(cfg.LightRAG)
		processor.WithLightRAG(lrClient)
		log.Infof("[main] LightRAG 已启用, URL: %s", cfg.LightRAG.URL)
	}

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

		// 学术检索Agent路由组
		// 提供学术论文检索、会话管理、候选论文列表查询和导入知识库功能
		researchAgent := apiV1.Group("/research-agent")
		researchAgent.Use(middleware.AuthMiddleware(jwtManager, userService)) // 需要JWT认证
		{
			// 创建Agent处理器，传入Agent服务和用户服务
			researchAgentHandler := handler.NewResearchAgentHandler(researchAgentService, userService)
			// GET /sessions - 查询用户的历史检索会话列表
			researchAgent.GET("/sessions", researchAgentHandler.ListSessions)
			// POST /sessions - 发起新的学术论文检索请求（阻塞模式）
			researchAgent.POST("/sessions", researchAgentHandler.RunSearch)
			// POST /sessions/stream - 发起新的学术论文检索请求（流式推送进度）
			researchAgent.POST("/sessions/stream", researchAgentHandler.RunSearchStream)
			// GET /sessions/:sessionId/candidates - 查询指定会话的候选论文列表
			researchAgent.GET("/sessions/:sessionId/candidates", researchAgentHandler.ListCandidates)
			// POST /candidates/:candidateId/import - 将候选论文下载并导入用户的知识库
			researchAgent.POST("/candidates/:candidateId/import", researchAgentHandler.ImportCandidate)
		}

		conversation := apiV1.Group("/users/conversation")
		conversation.Use(middleware.AuthMiddleware(jwtManager, userService))
		{
			conversationHandler := handler.NewConversationHandler(conversationService, chatService, userService)
			conversation.GET("", conversationHandler.GetConversations)
			conversation.POST("/compress", conversationHandler.CompressConversation)
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
