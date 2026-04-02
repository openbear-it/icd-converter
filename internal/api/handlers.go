package api

import (
	"net/http"
	"strconv"
	"strings"

	"icd-converter/internal/icd"

	"github.com/gin-gonic/gin"
)

// ConversionResponse is returned for single code conversion requests.
type ConversionResponse struct {
	Source   icd.ICDEntry   `json:"source"`
	Mappings []icd.ICDEntry `json:"mappings"`
}

// ExpandResponse is returned when expanding a parent code to show its children.
type ExpandResponse struct {
	Parent   icd.ICDEntry   `json:"parent"`
	Children []icd.ICDEntry `json:"children"`
}

// SearchResponse is returned for free-text search requests.
type SearchResponse struct {
	Query        string             `json:"query"`
	ICD9Results  []icd.SearchResult `json:"icd9_results"`
	ICD10Results []icd.SearchResult `json:"icd10_results"`
	CIPIResults  []icd.CIPISearchResult `json:"cipi_results"`
}

// ErrorResponse wraps an error message for the client.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Handler groups all HTTP handlers for the ICD API.
type Handler struct {
	store   *icd.Store
	grouper *icd.Grouper
}

// NewHandler returns a Handler backed by the given ICD store and DRG grouper.
func NewHandler(store *icd.Store, grouper *icd.Grouper) *Handler {
	return &Handler{store: store, grouper: grouper}
}

// RegisterRoutes mounts all /api/v1 routes on the given engine.
func RegisterRoutes(r *gin.Engine, h *Handler) {
	v1 := r.Group("/api/v1")
	{
		// ICD-9 endpoints
		v1.GET("/icd9/:code", h.GetICD9)
		v1.GET("/icd9/:code/to-icd10", h.ICD9ToICD10)
		v1.GET("/icd9/:code/expand", h.ExpandICD9)
		v1.GET("/icd9", h.ListICD9)

		// ICD-10 endpoints
		v1.GET("/icd10/:code", h.GetICD10)
		v1.GET("/icd10/:code/to-icd9", h.ICD10ToICD9)
		v1.GET("/icd10/:code/expand", h.ExpandICD10)
		v1.GET("/icd10", h.ListICD10)

		// CIPI endpoints
		v1.GET("/cipi/:code", h.GetCIPI)
		v1.GET("/cipi/:code/expand", h.ExpandCIPI)
		v1.GET("/cipi", h.ListCIPI)

		// DRG / MDC endpoints
		v1.GET("/drg/:code", h.GetDRG)
		v1.GET("/drg", h.ListDRG)
		v1.GET("/mdc", h.ListMDC)
		v1.GET("/mdc/:code", h.GetMDC)
		v1.POST("/drg/group", h.GroupDRG)

		// Bidirectional search
		v1.GET("/search", h.Search)
		v1.POST("/search/semantic", h.SemanticSearch)
	}
}

// GetICD9 godoc
// GET /api/v1/icd9/:code
// Returns a single ICD-9 entry.
func (h *Handler) GetICD9(c *gin.Context) {
	code := c.Param("code")
	entry, ok := h.store.LookupICD9(code)
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "ICD-9 code not found: " + code})
		return
	}
	c.JSON(http.StatusOK, entry)
}

// GetICD10 godoc
// GET /api/v1/icd10/:code
// Returns a single ICD-10 entry.
func (h *Handler) GetICD10(c *gin.Context) {
	code := c.Param("code")
	entry, ok := h.store.LookupICD10(code)
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "ICD-10 code not found: " + code})
		return
	}
	c.JSON(http.StatusOK, entry)
}

// ExpandICD9 godoc
// GET /api/v1/icd9/:code/expand
// Returns the parent ICD-9 entry together with all its children (codes that
// start with "<code>."). Useful for exploding a category code.
func (h *Handler) ExpandICD9(c *gin.Context) {
	code := c.Param("code")
	parent, ok := h.store.LookupICD9(code)
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "ICD-9 code not found: " + code})
		return
	}
	children := h.store.ChildrenICD9(code)
	c.JSON(http.StatusOK, ExpandResponse{Parent: parent, Children: children})
}

// ICD9ToICD10 godoc
// GET /api/v1/icd9/:code/to-icd10
// Converts an ICD-9 code to its ICD-10 equivalents.
func (h *Handler) ICD9ToICD10(c *gin.Context) {
	code := c.Param("code")
	src, ok := h.store.LookupICD9(code)
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "ICD-9 code not found: " + code})
		return
	}
	mappings, _ := h.store.ICD9ToICD10(code)
	c.JSON(http.StatusOK, ConversionResponse{Source: src, Mappings: mappings})
}

// ExpandICD10 godoc
// GET /api/v1/icd10/:code/expand
// Returns the parent ICD-10 entry together with all its children.
func (h *Handler) ExpandICD10(c *gin.Context) {
	code := c.Param("code")
	parent, ok := h.store.LookupICD10(code)
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "ICD-10 code not found: " + code})
		return
	}
	children := h.store.ChildrenICD10(code)
	c.JSON(http.StatusOK, ExpandResponse{Parent: parent, Children: children})
}

// ICD10ToICD9 godoc
// GET /api/v1/icd10/:code/to-icd9
// Converts an ICD-10 code to its ICD-9 equivalents.
func (h *Handler) ICD10ToICD9(c *gin.Context) {
	code := c.Param("code")
	src, ok := h.store.LookupICD10(code)
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "ICD-10 code not found: " + code})
		return
	}
	mappings, _ := h.store.ICD10ToICD9(code)
	c.JSON(http.StatusOK, ConversionResponse{Source: src, Mappings: mappings})
}

// ListICD9 godoc
// GET /api/v1/icd9?page=1&limit=20&category=Circulatory
// Returns a paginated list of ICD-9 entries, optionally filtered by category.
func (h *Handler) ListICD9(c *gin.Context) {
	all := h.store.AllICD9()
	c.JSON(http.StatusOK, paginateEntries(all, c))
}

// ListICD10 godoc
// GET /api/v1/icd10?page=1&limit=20&category=Circulatory
// Returns a paginated list of ICD-10 entries, optionally filtered by category.
func (h *Handler) ListICD10(c *gin.Context) {
	all := h.store.AllICD10()
	c.JSON(http.StatusOK, paginateEntries(all, c))
}

// Search godoc
// GET /api/v1/search?q=diabetes&version=both&cipi_type=
// Full-text search across ICD and CIPI codes.
// version: "icd9" | "icd10" | "cipi" | "both" (icd9+icd10, default) | "all"
// cipi_type: "" (both) | "diagnosi" | "procedura"  — only applied when searching CIPI
func (h *Handler) Search(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "query parameter 'q' is required"})
		return
	}
	version := strings.ToLower(c.DefaultQuery("version", "both"))
	cipiType := strings.ToLower(strings.TrimSpace(c.Query("cipi_type")))

	resp := SearchResponse{Query: q}
	switch version {
	case "icd9":
		resp.ICD9Results = h.store.SearchICD9(q)
	case "icd10":
		resp.ICD10Results = h.store.SearchICD10(q)
	case "cipi":
		resp.CIPIResults = h.store.SearchCIPI(q, cipiType)
	case "all":
		resp.ICD9Results, resp.ICD10Results = h.store.SearchAll(q)
		resp.CIPIResults = h.store.SearchCIPI(q, cipiType)
	default: // "both" — ICD-9 + ICD-10 (backward-compatible default)
		resp.ICD9Results, resp.ICD10Results = h.store.SearchAll(q)
	}
	if resp.ICD9Results == nil {
		resp.ICD9Results = []icd.SearchResult{}
	}
	if resp.ICD10Results == nil {
		resp.ICD10Results = []icd.SearchResult{}
	}
	if resp.CIPIResults == nil {
		resp.CIPIResults = []icd.CIPISearchResult{}
	}
	c.JSON(http.StatusOK, resp)
}

// paginatedResponse wraps paginated list responses.
type paginatedResponse struct {
	Total    int              `json:"total"`
	Page     int              `json:"page"`
	Limit    int              `json:"limit"`
	Items    []icd.ICDEntry  `json:"items"`
}

func paginateEntries(all []icd.ICDEntry, c *gin.Context) paginatedResponse {
	category := strings.TrimSpace(c.Query("category"))
	if category != "" {
		catLow := strings.ToLower(category)
		filtered := all[:0:0]
		for _, e := range all {
			if strings.ToLower(e.Category) == catLow {
				filtered = append(filtered, e)
			}
		}
		all = filtered
	}

	page := max(1, queryInt(c, "page", 1))
	limit := clamp(queryInt(c, "limit", 20), 1, 200)
	start := (page - 1) * limit
	end := start + limit
	if start >= len(all) {
		return paginatedResponse{Total: len(all), Page: page, Limit: limit, Items: []icd.ICDEntry{}}
	}
	if end > len(all) {
		end = len(all)
	}
	return paginatedResponse{Total: len(all), Page: page, Limit: limit, Items: all[start:end]}
}

func queryInt(c *gin.Context, key string, def int) int {
	s := c.Query(key)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ── DRG handlers ──────────────────────────────────────────────────────────────

// drgPage wraps a paginated DRG list response.
type drgPage struct {
	Total int            `json:"total"`
	Page  int            `json:"page"`
	Limit int            `json:"limit"`
	Items []icd.DRGEntry `json:"items"`
}

// DRGSearchResponse wraps a DRG text-search response.
type DRGSearchResponse struct {
	Query string               `json:"query"`
	Items []icd.DRGSearchResult `json:"items"`
}

// GetDRG godoc
// GET /api/v1/drg/:code
// Returns a single MS-DRG entry. The code may be supplied with or without
// leading zeros (e.g. "1", "01", and "001" all resolve to DRG 001).
func (h *Handler) GetDRG(c *gin.Context) {
	code := c.Param("code")
	entry, ok := h.store.LookupDRG(code)
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "DRG code not found: " + code})
		return
	}
	c.JSON(http.StatusOK, entry)
}

// ListDRG godoc
// GET /api/v1/drg?q=heart&mdc=05&type=MED&page=1&limit=20
// When q is provided it performs a BM25 text search.
// Otherwise it returns a paginated list, optionally filtered by mdc and/or type.
func (h *Handler) ListDRG(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	mdcFilter := strings.TrimSpace(c.Query("mdc"))
	typeFilter := strings.ToUpper(strings.TrimSpace(c.Query("type")))

	if q != "" {
		results := h.store.SearchDRG(q, mdcFilter)
		if results == nil {
			results = []icd.DRGSearchResult{}
		}
		c.JSON(http.StatusOK, DRGSearchResponse{Query: q, Items: results})
		return
	}

	all := h.store.AllDRG()
	if mdcFilter != "" {
		filtered := all[:0:0]
		for _, e := range all {
			if e.MDC == mdcFilter {
				filtered = append(filtered, e)
			}
		}
		all = filtered
	}
	if typeFilter != "" {
		filtered := all[:0:0]
		for _, e := range all {
			if e.Type == typeFilter {
				filtered = append(filtered, e)
			}
		}
		all = filtered
	}

	page := max(1, queryInt(c, "page", 1))
	limit := clamp(queryInt(c, "limit", 20), 1, 200)
	start := (page - 1) * limit
	end := start + limit
	if start >= len(all) {
		c.JSON(http.StatusOK, drgPage{Total: len(all), Page: page, Limit: limit, Items: []icd.DRGEntry{}})
		return
	}
	if end > len(all) {
		end = len(all)
	}
	c.JSON(http.StatusOK, drgPage{Total: len(all), Page: page, Limit: limit, Items: all[start:end]})
}

// ListMDC godoc
// GET /api/v1/mdc
// Returns all MDC entries ordered by code.
func (h *Handler) ListMDC(c *gin.Context) {
	all := h.store.AllMDC()
	c.JSON(http.StatusOK, all)
}

// GetMDC godoc
// GET /api/v1/mdc/:code
// Returns a single MDC entry together with the DRG codes that belong to it.
func (h *Handler) GetMDC(c *gin.Context) {
	code := c.Param("code")
	entry, ok := h.store.LookupMDC(code)
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "MDC code not found: " + code})
		return
	}
	drgs := []icd.DRGEntry{}
	for _, d := range h.store.AllDRG() {
		if d.MDC == code {
			drgs = append(drgs, d)
		}
	}
	c.JSON(http.StatusOK, gin.H{"mdc": entry, "drgs": drgs})
}

// ── CIPI handlers ─────────────────────────────────────────────────────────────

// CIPIExpandResponse is returned when expanding a CIPI parent code.
type CIPIExpandResponse struct {
	Parent   icd.CIPIEntry   `json:"parent"`
	Children []icd.CIPIEntry `json:"children"`
}

// GetCIPI godoc
// GET /api/v1/cipi/:code
// Returns a single CIPI entry.
func (h *Handler) GetCIPI(c *gin.Context) {
	code := c.Param("code")
	entry, ok := h.store.LookupCIPI(code)
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "CIPI code not found: " + code})
		return
	}
	c.JSON(http.StatusOK, entry)
}

// ExpandCIPI godoc
// GET /api/v1/cipi/:code/expand
// Returns the parent CIPI entry together with all its direct children.
func (h *Handler) ExpandCIPI(c *gin.Context) {
	code := c.Param("code")
	parent, ok := h.store.LookupCIPI(code)
	if !ok {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "CIPI code not found: " + code})
		return
	}
	children := h.store.ChildrenCIPI(code)
	c.JSON(http.StatusOK, CIPIExpandResponse{Parent: parent, Children: children})
}

// ListCIPI godoc
// GET /api/v1/cipi?page=1&limit=20&type=procedura
// Returns a paginated list of CIPI entries, optionally filtered by type.
func (h *Handler) ListCIPI(c *gin.Context) {
	all := h.store.AllCIPI()
	typeFilter := strings.ToLower(strings.TrimSpace(c.Query("type")))
	if typeFilter != "" {
		filtered := all[:0:0]
		for _, e := range all {
			if e.Type == typeFilter {
				filtered = append(filtered, e)
			}
		}
		all = filtered
	}

	page := max(1, queryInt(c, "page", 1))
	limit := clamp(queryInt(c, "limit", 20), 1, 200)
	start := (page - 1) * limit
	end := start + limit

	type cipiPage struct {
		Total int             `json:"total"`
		Page  int             `json:"page"`
		Limit int             `json:"limit"`
		Items []icd.CIPIEntry `json:"items"`
	}
	if start >= len(all) {
		c.JSON(http.StatusOK, cipiPage{Total: len(all), Page: page, Limit: limit, Items: []icd.CIPIEntry{}})
		return
	}
	if end > len(all) {
		end = len(all)
	}
	c.JSON(http.StatusOK, cipiPage{Total: len(all), Page: page, Limit: limit, Items: all[start:end]})
}
