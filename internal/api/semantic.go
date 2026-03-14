package api

import (
	"log"
	"net/http"
	"strings"
	"time"

	"icd-converter/internal/embed"
	"icd-converter/internal/icd"

	"github.com/gin-gonic/gin"
)

// embedBuilder is the configured embedding builder (nil when EMBED_MODEL is not set).
var embedBuilder *embed.Builder

// embedICD9 and embedICD10 hold the precomputed embedding indexes.
var embedICD9 *embed.Index
var embedICD10 *embed.Index

// SetEmbedder injects the embedding builder and precomputed indexes.
func SetEmbedder(builder *embed.Builder, icd9 *embed.Index, icd10 *embed.Index) {
	embedBuilder = builder
	embedICD9 = icd9
	embedICD10 = icd10
}

// SemanticSearchResponse is returned by the semantic search endpoint.
type SemanticSearchResponse struct {
	Query        string               `json:"query"`
	ICD9Results  []embed.SearchResult `json:"icd9_results"`
	ICD10Results []embed.SearchResult `json:"icd10_results"`
	Model        string               `json:"model"`
}

// SemanticSearchRequest is the JSON body for the semantic search endpoint.
type SemanticSearchRequest struct {
	Query   string `json:"q"   binding:"required"`
	Version string `json:"version"`
	Limit   int    `json:"limit"`
	// Mode controls which engine is used: "" or "auto" = prefer embedding, fall back to heuristic;
	// "heuristic" = always keyword search; "embedding" = fail if embedding index not ready.
	Mode string `json:"mode"`
}

// SemanticSearch godoc
// POST /api/v1/search/semantic
// Body: { "q": "...", "version": "both", "limit": 10, "mode": "auto" }
// Ranks ICD codes by embedding cosine similarity or keyword heuristic.
func (h *Handler) SemanticSearch(c *gin.Context) {
	var req SemanticSearchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	q := strings.TrimSpace(req.Query)
	if q == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "field 'q' is required"})
		return
	}

	if req.Limit <= 0 {
		req.Limit = 10
	}
	limit := clamp(req.Limit, 1, 50)
	version := strings.ToLower(req.Version)
	if version == "" {
		version = "both"
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "auto"
	}

	start := time.Now()

	// "embedding" mode requested but index not ready → return error.
	if mode == "embedding" && embedBuilder == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{
			Error: "embedding index not available: set LLM_EMBED_MODEL to enable it",
		})
		return
	}

	// Use heuristic when: mode=heuristic, or mode=auto and no embedding index.
	if mode == "heuristic" || (mode == "auto" && embedBuilder == nil) {
		log.Printf("search/semantic: mode=heuristic version=%s limit=%d query=%q", version, limit, q)
		var icd9Res, icd10Res []embed.SearchResult
		if version != "icd10" {
			icd9Res = heuristicToEmbedResults(h.store.SearchICD9(q), limit)
		}
		if version != "icd9" {
			icd10Res = heuristicToEmbedResults(h.store.SearchICD10(q), limit)
		}
		if icd9Res == nil {
			icd9Res = []embed.SearchResult{}
		}
		if icd10Res == nil {
			icd10Res = []embed.SearchResult{}
		}
		log.Printf("search/semantic: done mode=heuristic icd9=%d icd10=%d elapsed=%s",
			len(icd9Res), len(icd10Res), time.Since(start).Round(time.Millisecond))
		c.JSON(http.StatusOK, SemanticSearchResponse{
			Query:        q,
			ICD9Results:  icd9Res,
			ICD10Results: icd10Res,
			Model:        "heuristic",
		})
		return
	}

	log.Printf("search/semantic: mode=embedding model=%q version=%s limit=%d query=%q",
		embedBuilder.ModelName(), version, limit, q)

	queryVec, err := embedBuilder.EmbedOne(c.Request.Context(), q)
	if err != nil {
		log.Printf("search/semantic: ERROR embedding query=%q err=%v elapsed=%s",
			q, err, time.Since(start).Round(time.Millisecond))
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "embedding failed: " + err.Error()})
		return
	}

	var icd9Res, icd10Res []embed.SearchResult

	if version != "icd10" && embedICD9 != nil {
		icd9Res = embedICD9.Search(queryVec, limit)
	}
	if version != "icd9" && embedICD10 != nil {
		icd10Res = embedICD10.Search(queryVec, limit)
	}
	if icd9Res == nil {
		icd9Res = []embed.SearchResult{}
	}
	if icd10Res == nil {
		icd10Res = []embed.SearchResult{}
	}

	log.Printf("search/semantic: done mode=embedding model=%q icd9=%d icd10=%d elapsed=%s",
		embedBuilder.ModelName(), len(icd9Res), len(icd10Res), time.Since(start).Round(time.Millisecond))
	c.JSON(http.StatusOK, SemanticSearchResponse{
		Query:        q,
		ICD9Results:  icd9Res,
		ICD10Results: icd10Res,
		Model:        embedBuilder.ModelName(),
	})
}

// heuristicToEmbedResults converts keyword search results to the embed.SearchResult
// type, normalising scores to the [0, 1] range so they are comparable to cosine
// similarity values returned by the embedding index.
func heuristicToEmbedResults(results []icd.SearchResult, limit int) []embed.SearchResult {
	if len(results) > limit {
		results = results[:limit]
	}
	var maxScore float64
	for _, r := range results {
		if r.Score > maxScore {
			maxScore = r.Score
		}
	}
	out := make([]embed.SearchResult, len(results))
	for i, r := range results {
		score := r.Score
		if maxScore > 0 {
			score = r.Score / maxScore
		}
		out[i] = embed.SearchResult{ICDEntry: r.ICDEntry, Score: score}
	}
	return out
}
