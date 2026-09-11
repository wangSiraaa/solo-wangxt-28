package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"quotaapp/internal/api"
	"quotaapp/internal/db"
	"quotaapp/internal/ledger"
	"quotaapp/internal/store"
	"quotaapp/internal/sweeper"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func main() {
	ctx := context.Background()
	dsn := env("DATABASE_DSN",
		"postgres://quota@127.0.0.1:55432/quota_ledger?sslmode=disable")
	pg, err := db.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	if err := db.Migrate(ctx, pg); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	st, err := store.New(ctx, store.Config{
		Endpoint:  env("MINIO_ENDPOINT", "http://127.0.0.1:9000"),
		AccessKey: env("MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: env("MINIO_SECRET_KEY", "minioadmin"),
		Bucket:    env("MINIO_BUCKET", "quota-objects"),
	})
	if err != nil {
		log.Fatalf("init minio: %v", err)
	}

	repo := ledger.New(pg)
	interval := time.Duration(envInt("SWEEP_INTERVAL_MS", 5000)) * time.Millisecond
	sw := sweeper.New(repo, st, interval)
	go sw.Run(ctx)

	gin.SetMode(env("GIN_MODE", "debug"))
	r := gin.Default()
	// CORS：浏览器前端直连本服务（分片直传 MinIO 由 bucket CORS 负责）
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type,Idempotency-Key")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	h := api.New(repo, st, &sweepAdapter{sw},
		time.Duration(envInt("SESSION_TTL_SECONDS", 300))*time.Second,
		int64(envInt("MIN_PART_SIZE", 5*1024*1024)))
	h.Register(r)

	addr := env("HTTP_ADDR", "127.0.0.1:8080")
	log.Printf("quota service listening on %s (sweep every %s, session ttl %ds)",
		addr, interval, envInt("SESSION_TTL_SECONDS", 300))
	if err := r.Run(addr); err != nil {
		log.Fatal(err)
	}
}

// sweepAdapter 把 sweeper.TickResult 适配到 api 层需要的形状，避免 api 依赖 sweeper。
type sweepAdapter struct{ sw *sweeper.Sweeper }

func (a *sweepAdapter) Tick(ctx context.Context) api.SweepResult {
	r := a.sw.Tick(ctx)
	return api.SweepResult{
		SessionsExpired:  r.SessionsExpired,
		ObjectsReclaimed: r.ObjectsReclaimed,
		BytesReclaimed:   r.BytesReclaimed,
	}
}
