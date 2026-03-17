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

// embedICD9, embedICD10 and embedCIPI hold the precomputed embedding indexes.
var embedICD9 *embed.Index
var embedICD10 *embed.Index
var embedCIPI *embed.Index

// SetEmbedder injects the embedding builder and precomputed indexes.
func SetEmbedder(builder *embed.Builder, icd9 *embed.Index, icd10 *embed.Index, cipi *embed.Index) {
	embedBuilder = builder
	embedICD9 = icd9
	embedICD10 = icd10
	embedCIPI = cipi
}

// SemanticSearchResponse is returned by the semantic search endpoint.
type SemanticSearchResponse struct {
	Query        string               `json:"query"`
	ICD9Results  []embed.SearchResult `json:"icd9_results"`
	ICD10Results []embed.SearchResult `json:"icd10_results"`
	CIPIResults  []embed.SearchResult `json:"cipi_results"`
	Model        string               `json:"model"`
}

// SemanticSearchRequest is the JSON body for the semantic search endpoint.
type SemanticSearchRequest struct {
	Query    string `json:"q"   binding:"required"`
	Version  string `json:"version"`
	Limit    int    `json:"limit"`
	// Mode controls which engine is used: "" or "auto" = prefer embedding, fall back to heuristic;
	// "heuristic" = always keyword search; "embedding" = fail if embedding index not ready.
	Mode     string `json:"mode"`
	// CIPIType restricts CIPI results: "" or "all" = both; "diagnosi"; "procedura"
	CIPIType string `json:"cipi_type"`
}

// SemanticSearch godoc
// POST /api/v1/search/semantic
// Body: { "q": "...", "version": "all", "limit": 10, "mode": "auto", "cipi_type": "" }
// Ranks ICD/CIPI codes by embedding cosine similarity or keyword heuristic.
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
		version = "all"
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "auto"
	}
	cipiType := strings.ToLower(strings.TrimSpace(req.CIPIType))

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
		var cipiRes []embed.SearchResult
		if version != "icd10" && version != "cipi" {
			icd9Res = heuristicToEmbedResults(h.store.SearchICD9(q), limit)
		}
		if version != "icd9" && version != "cipi" {
			icd10Res = heuristicToEmbedResults(h.store.SearchICD10(q), limit)
		}
		if version == "cipi" || version == "all" || version == "both" {
			cipiRes = heuristicCIPIToEmbedResults(h.store.SearchCIPI(q, cipiType), limit)
		}
		icd9Res = emptyIfNil(icd9Res)
		icd10Res = emptyIfNil(icd10Res)
		cipiRes = emptyIfNil(cipiRes)
		log.Printf("search/semantic: done mode=heuristic icd9=%d icd10=%d cipi=%d elapsed=%s",
			len(icd9Res), len(icd10Res), len(cipiRes), time.Since(start).Round(time.Millisecond))
		c.JSON(http.StatusOK, SemanticSearchResponse{
			Query:        q,
			ICD9Results:  icd9Res,
			ICD10Results: icd10Res,
			CIPIResults:  cipiRes,
			Model:        "heuristic",
		})
		return
	}

	log.Printf("search/semantic: mode=embedding model=%q version=%s limit=%d query=%q",
		embedBuilder.ModelName(), version, limit, q)

	// Split long clinical texts into clauses and embed each independently.
	// For short queries a single embedding is used (same behaviour as before).
	chunks := embed.SplitClinicalText(q, 10)
	queryVecs := make([][]float32, 0, len(chunks))
	for _, chunk := range chunks {
		vec, err := embedBuilder.EmbedOne(c.Request.Context(), chunk)
		if err != nil {
			log.Printf("search/semantic: ERROR embedding chunk=%q err=%v elapsed=%s",
				chunk, err, time.Since(start).Round(time.Millisecond))
			c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "embedding failed: " + err.Error()})
			return
		}
		queryVecs = append(queryVecs, vec)
	}
	if len(chunks) > 1 {
		log.Printf("search/semantic: multi-query mode chunks=%d", len(chunks))
	}

	var icd9Res, icd10Res, cipiRes []embed.SearchResult

	if version != "icd10" && version != "cipi" && embedICD9 != nil {
		icd9Res = embedICD9.MultiQuerySearch(queryVecs, limit)
	}
	if version != "icd9" && version != "cipi" && embedICD10 != nil {
		icd10Res = embedICD10.MultiQuerySearch(queryVecs, limit)
	}
	if (version == "cipi" || version == "all" || version == "both") && embedCIPI != nil {
		raw := embedCIPI.MultiQuerySearch(queryVecs, limit)
		cipiRes = filterByCIPIType(raw, cipiType)
	}

	icd9Res = emptyIfNil(icd9Res)
	icd10Res = emptyIfNil(icd10Res)
	cipiRes = emptyIfNil(cipiRes)

	log.Printf("search/semantic: done mode=embedding model=%q icd9=%d icd10=%d cipi=%d elapsed=%s",
		embedBuilder.ModelName(), len(icd9Res), len(icd10Res), len(cipiRes), time.Since(start).Round(time.Millisecond))
	c.JSON(http.StatusOK, SemanticSearchResponse{
		Query:        q,
		ICD9Results:  icd9Res,
		ICD10Results: icd10Res,
		CIPIResults:  cipiRes,
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

// heuristicCIPIToEmbedResults converts CIPI keyword results into embed.SearchResult
// by mapping CIPIEntry → ICDEntry (Category = Type, Mappings = nil).
func heuristicCIPIToEmbedResults(results []icd.CIPISearchResult, limit int) []embed.SearchResult {
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
			score /= maxScore
		}
		out[i] = embed.SearchResult{
			ICDEntry: icd.ICDEntry{
				Code:        r.Code,
				Description: r.Description,
				Category:    r.Type,
				Mappings:    []string{},
			},
			Score: score,
		}
	}
	return out
}

// filterByCIPIType filters embedding search results by CIPI type ("diagnosi" | "procedura").
// In the CIPI embedding index, the ICDEntry.Category field holds the CIPI type.
// Pass "" to skip filtering.
func filterByCIPIType(results []embed.SearchResult, cipiType string) []embed.SearchResult {
	if cipiType == "" || cipiType == "all" {
		return results
	}
	filtered := results[:0:0]
	for _, r := range results {
		if r.Category == cipiType {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// emptyIfNil returns an empty slice if the input is nil (avoids null in JSON).
func emptyIfNil(s []embed.SearchResult) []embed.SearchResult {
	if s == nil {
		return []embed.SearchResult{}
	}
	return s
}
