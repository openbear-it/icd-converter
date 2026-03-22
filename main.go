package main

import (
	"bufio"
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"icd-converter/internal/api"
	"icd-converter/internal/db"
	embedpkg "icd-converter/internal/embed"
	"icd-converter/internal/icd"
	"icd-converter/internal/llm"

	"github.com/gin-gonic/gin"
)

//go:embed web
var webFS embed.FS

//go:embed data/official/*.csv
var officialDataFS embed.FS

func main() {
	loadDotEnv(".env")

	// ── Configuration from environment ──────────────────────────────────────
	port   := envOr("PORT", "8080")
	dbPath := envOr("ICD_DB_PATH", "icd.db")
	baseURL := os.Getenv("LLM_BASE_URL")
	apiKey  := os.Getenv("LLM_API_KEY")

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
	log.Printf("ICD store loaded: %d ICD-9 codes, %d ICD-10 codes, %d CIPI codes",
		len(store.AllICD9()), len(store.AllICD10()), len(store.AllCIPI()))

	// ── LLM Engine (for inference and query expansion) ────────────────────────
	llmModel := envOr("LLM_MODEL", "gpt-4o-mini")
	var llmEngine *llm.Engine
	if apiKey != "" || baseURL != "" {
		llmEngine = llm.NewEngine(llm.Config{
			APIKey:               apiKey,
			BaseURL:              baseURL,
			Model:                llmModel,
			TimeoutSeconds:       envOrInt("LLM_TIMEOUT", 60),
			ExpandTimeoutSeconds: envOrInt("LLM_EXPAND_TIMEOUT", 10),
		}, store)
		log.Printf("LLM engine ready: model=%s base_url=%q", llmModel, baseURL)
		// Pre-warm: ask Ollama to load the model into memory now, so the first
		// real expansion request doesn't pay the cold-start penalty (~10-15s).
		go func() {
			wctx, wcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer wcancel()
			if _, err := llmEngine.ExpandQuery(wctx, "warm-up"); err == nil {
				log.Printf("LLM model warm-up done: model=%s", llmModel)
			}
		}()
	} else {
		log.Printf("LLM engine disabled: set OPENAI_API_KEY (+ LLM_BASE_URL for Ollama) and LLM_MODEL to enable query expansion")
	}

	// ── Semantic Embedding Search ─────────────────────────────────────────────
	embedModel := os.Getenv("LLM_EMBED_MODEL")
	if embedModel != "" {
		embedBaseURL := envOr("LLM_EMBED_BASE_URL", baseURL)
		embedAPIKey := envOr("LLM_EMBED_API_KEY", apiKey)
		// EnrichText adds category/chapter context to each indexed entry before
		// embedding — this significantly improves semantic recall.
		// Set LLM_EMBED_ENRICH_TEXT=false to disable (uses separate cache namespace).
		enrichText := os.Getenv("LLM_EMBED_ENRICH_TEXT") != "false"
		builder := embedpkg.NewBuilder(embedpkg.Config{
			APIKey:     embedAPIKey,
			BaseURL:    embedBaseURL,
			Model:      embedModel,
			EnrichText: enrichText,
		})
		versionID, err := db.GetActiveVersionID(sqldb)
		if err != nil {
			log.Fatalf("get version id: %v", err)
		}
		ctx := context.Background()
		idx9, err := builder.BuildOrLoad(ctx, sqldb, versionID, "icd9", store.AllICD9())
		if err != nil {
			log.Fatalf("embed icd9: %v", err)
		}
		idx10, err := builder.BuildOrLoad(ctx, sqldb, versionID, "icd10", store.AllICD10())
		if err != nil {
			log.Fatalf("embed icd10: %v", err)
		}
		idxCIPI, err := builder.BuildOrLoad(ctx, sqldb, versionID, "cipi", cipiToICDEntries(store.AllCIPI()))
		if err != nil {
			log.Fatalf("embed cipi: %v", err)
		}
		api.SetEmbedder(builder, idx9, idx10, idxCIPI)
		// Inject LLM engine for query expansion (requires a chat-capable model).
		if llmEngine != nil {
			api.SetSemanticLLMEngine(llmEngine)
			log.Printf("Semantic query expansion enabled: expand_model=%s", llmModel)
		}
		log.Printf("Semantic search ready: model=%s  enrich=%v  icd9=%d  icd10=%d  cipi=%d vectors",
			embedModel, enrichText, idx9.Len(), idx10.Len(), idxCIPI.Len())
	} else {
		log.Printf("Semantic search disabled (set LLM_EMBED_MODEL to enable, e.g. LLM_EMBED_MODEL=all-minilm)")
	}

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

func envOrInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
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

// loadDotEnv reads key=value pairs from path and sets them as environment
// variables, skipping blank lines and lines starting with '#'.
// Variables already present in the environment are never overwritten.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // .env is optional
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// strip optional surrounding quotes
		if len(value) >= 2 &&
			((value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}

// cipiToICDEntries converts CIPI entries to ICDEntry slices so they can be used
// with the shared embedding index infrastructure.  The Type field is mapped to
// Category; Mappings is left empty.
func cipiToICDEntries(entries []icd.CIPIEntry) []icd.ICDEntry {
	out := make([]icd.ICDEntry, len(entries))
	for i, e := range entries {
		out[i] = icd.ICDEntry{
			Code:        e.Code,
			Description: e.Description,
			Category:    e.Type,
			Mappings:    []string{},
		}
	}
	return out
}
