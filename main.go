package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"

	"icd-converter/internal/api"
	"icd-converter/internal/db"
	"icd-converter/internal/llm"

	"github.com/gin-gonic/gin"
)

//go:embed web
var webFS embed.FS

//go:embed data/official/*.csv
var officialDataFS embed.FS

func main() {
	// ── Configuration from environment ──────────────────────────────────────
	port := envOr("PORT", "8080")
	apiKey := os.Getenv("OPENAI_API_KEY")      // leave empty for heuristic mode
	baseURL := os.Getenv("LLM_BASE_URL")        // override for Ollama / LM Studio
	model := envOr("LLM_MODEL", "gpt-4o-mini") // model name
	dbPath := envOr("ICD_DB_PATH", "icd.db")

	// ── Database: open, migrate schema, seed on first run ────────────────────
	sqldb, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("db open: %v", err)
	}
	defer sqldb.Close()

	seeded, err := db.IsSeeded(sqldb)
	if err != nil {
		log.Fatalf("db check seed: %v", err)
	}
	if !seeded {
		if err := db.Seed(sqldb, db.SeedOptions{
			VersionName: "ICD-9-CM + ICD-10-IM v.GAMMA 2.1",
			Year:        2026,
			SourceURL:   "https://www.salute.gov.it",
			DataFS:      officialDataFS,
		}); err != nil {
			log.Fatalf("db seed: %v", err)
		}
	}

	// ── ICD Store (loaded from SQLite) ────────────────────────────────────────
	store, err := db.LoadStore(sqldb)
	if err != nil {
		log.Fatalf("load store: %v", err)
	}
	log.Printf("ICD store loaded: %d ICD-9 codes, %d ICD-10 codes",
		len(store.AllICD9()), len(store.AllICD10()))

	// ── LLM Engine ────────────────────────────────────────────────────────────
	engine := llm.NewEngine(llm.Config{
		APIKey:         apiKey,
		BaseURL:        baseURL,
		Model:          model,
		TimeoutSeconds: 60,
	}, store)
	api.SetLLMEngine(engine)

	mode := "heuristic (no OPENAI_API_KEY set)"
	if apiKey != "" {
		mode = "LLM (" + model + ")"
		if baseURL != "" {
			mode += " @ " + baseURL
		}
	}
	log.Printf("Inference mode: %s", mode)

	// ── HTTP Router ───────────────────────────────────────────────────────────
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.Default()

	// CORS middleware (permissive for development)
	r.Use(corsMiddleware())

	// API routes
	h := api.NewHandler(store)
	api.RegisterRoutes(r, h)

	// Serve static web UI from embedded FS
	webStatic, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("failed to create sub FS for web: %v", err)
	}
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/ui/")
	})
	r.StaticFS("/ui", http.FS(webStatic))

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	log.Printf("Starting ICD Converter on http://localhost:%s", port)
	log.Printf("  API docs:  http://localhost:%s/api/v1/", port)
	log.Printf("  Test UI:   http://localhost:%s/ui/", port)
	log.Printf("  Database:  %s", dbPath)

	if err := r.Run(":" + port); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
