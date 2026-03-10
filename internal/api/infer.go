package api

import (
	"net/http"

	"icd-converter/internal/llm"

	"github.com/gin-gonic/gin"
)

// llmEngine is set at startup via SetLLMEngine.
var llmEngine *llm.Engine

// SetLLMEngine injects the LLM engine into the API layer.
func SetLLMEngine(e *llm.Engine) {
	llmEngine = e
}

// Infer godoc
// POST /api/v1/infer
// Body: { "descriptions": ["patient with chest pain and shortness of breath"], "max_results": 5 }
// Returns LLM-derived ICD-9 and ICD-10 suggestions.
func (h *Handler) Infer(c *gin.Context) {
	var req llm.InferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if llmEngine == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "LLM engine not initialized"})
		return
	}
	resp, err := llmEngine.Infer(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}
