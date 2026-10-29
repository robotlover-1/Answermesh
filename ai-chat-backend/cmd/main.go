package main

import (
	"ai-chat-backend/pkg/config"
	"ai-chat-backend/pkg/log"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"ai-chat-backend/pkg/controllers"
	mysqlpkg "ai-chat-backend/pkg/db/mysql"
	"ai-chat-backend/pkg/middlewares"
	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

type ChatGPTWebServer struct {
	config *config.Config
	log    log.ILogger
}

func (r *ChatGPTWebServer) Run(ctx context.Context) error {
	go r.httpServer(ctx)
	return nil
}

func (r *ChatGPTWebServer) httpServer(ctx context.Context) {
	if err := mysqlpkg.InitUsersTable(); err != nil {
		r.log.FatalF("init users table: %v", err)
	}
	chatService, err := controllers.NewChatService(r.config, r.log)
	if err != nil {
		r.log.Fatal(err)
	}

	addr := fmt.Sprintf("%s:%d", r.config.Http.IP, r.config.Http.Port)
	r.log.InfoF("ChatGPT Web Server on: %s", addr)
	server := &http.Server{
		Addr: addr,
	}
	engine, err := middlewares.NewEngine(r.config.Http.TrustedProxies)
	if err != nil {
		r.log.Fatal(err)
	}
	entry := engine
	chat := entry.Group("/api")
	chat.Use(middlewares.RateLimitMiddleware(rate.Limit(10), 10))
	chat.POST("/chat-process", middlewares.AuthMiddleware(), chatService.ChatProcess)

	// 会话/消息（tenant/user 由 backend 身份令牌派生，见 controllers/sessions.go）
	sessions := chat.Group("/v1/sessions")
	sessions.Use(middlewares.AuthMiddleware())
	sessions.POST("", chatService.CreateSession)
	sessions.GET("", chatService.ListSessions)
	sessions.DELETE("/:id", chatService.DeleteSession)
	sessions.GET("/:id/messages", chatService.ListMessages)
	sessions.POST("/:id/messages", chatService.AppendConversation)
	chat.POST("/config", func(ctx *gin.Context) {
		ctx.JSON(200, gin.H{
			"status": "Success",
			"data": map[string]string{
				"apiModel":   "ChatGPTAPI",
				"socksProxy": "",
				"model":      config.GetConfig().Chat.Model,
			},
		})
	})
	chat.POST("/session", chatService.Session)
	chat.POST("/v1/user/login", chatService.Login)
	chat.GET("/health", func(c *gin.Context) {})
	chat.GET("/readyz", controllers.ReadyzHandler())
	fs := http.FileServer(http.Dir("www"))
	entry.NoRoute(func(ctx *gin.Context) {
		fs.ServeHTTP(ctx.Writer, ctx.Request)
	})
	entry.GET("/", func(ctx *gin.Context) {
		http.ServeFile(ctx.Writer, ctx.Request, "www/index.html")
	})
	server.Handler = entry
	go func(ctx context.Context) {
		<-ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			r.log.InfoF("Server shutdown with error %v", err)
		}
	}(ctx)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		r.log.FatalF("Server listen and serve error %v", err)
	}
}

var (
	configFile = flag.String("config", "dev.config.yaml", "")
)

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Kill, os.Interrupt)
	defer stop()

	config.InitConfig(*configFile)
	cnf := config.GetConfig()
	log.InfoF("config: http=%s:%d model=%s auth=%v log.level=%s",
		cnf.Http.IP, cnf.Http.Port, cnf.Chat.Model, cnf.Auth.Enabled, cnf.Log.Level)

	//初始化日志
	log.SetLevel(cnf.Log.Level)
	log.SetOutput(log.GetRotateWriter(cnf.Log.LogPath))
	log.SetPrintCaller(true)

	logger := log.NewLogger()
	logger.SetLevel(cnf.Log.Level)
	logger.SetOutput(log.GetRotateWriter(cnf.Log.LogPath))
	logger.SetPrintCaller(true)

	webServer := &ChatGPTWebServer{
		config: cnf,
		log:    logger,
	}
	webServer.Run(ctx)
	<-ctx.Done()
}
