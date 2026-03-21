package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"icd-converter/internal/icd"

	openai "github.com/sashabaranov/go-openai"
)

// Config holds the configuration for the LLM engine.
type Config struct {
	// APIKey is the OpenAI (or compatible) API key. Leave empty to use local/mock mode.
	APIKey string
	// BaseURL overrides the default OpenAI base URL (e.g. for Ollama or LM Studio).
	BaseURL string
	// Model is the model name to use (default: gpt-4o-mini).
	Model string
	// TimeoutSeconds is the per-request timeout (default: 60).
	TimeoutSeconds int
}

// InferRequest is the input for an LLM inference call.
type InferRequest struct {
	// Descriptions is a list of free-text clinical descriptions (diagnoses, procedures, symptoms…).
	Descriptions []string `json:"descriptions" binding:"required,min=1"`
	// MaxResults limits the total number of ICD suggestions returned per version (default: 5).
	MaxResults int `json:"max_results"`
}

// ICDSuggestion is a single ranked ICD code suggestion produced by the LLM.
type ICDSuggestion struct {
	Code        string  `json:"code"`
	Description string  `json:"description"`
	Category    string  `json:"category"`
	Confidence  float64 `json:"confidence"` // 0.0 – 1.0 as reported by the LLM
	Rationale   string  `json:"rationale"`
}

// InferResponse contains the LLM-derived ICD suggestions.
type InferResponse struct {
	Descriptions []string        `json:"descriptions"`
	ICD9          []ICDSuggestion `json:"icd9_suggestions"`
	ICD10         []ICDSuggestion `json:"icd10_suggestions"`
	Mode          string          `json:"mode"` // "llm" or "heuristic"
	Model         string          `json:"model,omitempty"`
}

// Engine wraps an OpenAI-compatible client for ICD inference.
type Engine struct {
	cfg    Config
	client *openai.Client
	store  *icd.Store
}

// NewEngine creates a new LLM Engine. If APIKey is empty the engine falls back
// to the built-in heuristic keyword matcher.
func NewEngine(cfg Config, store *icd.Store) *Engine {
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 60
	}
	var client *openai.Client
	if cfg.APIKey != "" {
		clientCfg := openai.DefaultConfig(cfg.APIKey)
		if cfg.BaseURL != "" {
			clientCfg.BaseURL = cfg.BaseURL
		}
		client = openai.NewClientWithConfig(clientCfg)
	}
	return &Engine{cfg: cfg, client: client, store: store}
}

// Infer returns ICD suggestions for the given clinical descriptions.
// It uses the LLM if configured, otherwise falls back to heuristic search.
func (e *Engine) Infer(ctx context.Context, req InferRequest) (*InferResponse, error) {
	if req.MaxResults <= 0 {
		req.MaxResults = 5
	}
	if e.client == nil {
		return e.heuristicInfer(req)
	}
	return e.llmInfer(ctx, req)
}

// llmInfer delegates to the OpenAI-compatible API.
func (e *Engine) llmInfer(ctx context.Context, req InferRequest) (*InferResponse, error) {
	timeout := time.Duration(e.cfg.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	log.Printf("llm: calling model=%q descriptions=%d max_results=%d", e.cfg.Model, len(req.Descriptions), req.MaxResults)

	systemPrompt := `You are a clinical coding assistant specialized in ICD-9-CM and ICD-10-CM coding.
Given a list of clinical descriptions (diagnoses, procedures, symptoms, etc.), identify the most appropriate ICD-9 and ICD-10 codes.

Respond ONLY with a valid JSON object matching this exact schema:
{
  "icd9_suggestions": [
    {"code": "401.9", "description": "...", "category": "...", "confidence": 0.95, "rationale": "..."}
  ],
  "icd10_suggestions": [
    {"code": "I10", "description": "...", "category": "...", "confidence": 0.95, "rationale": "..."}
  ]
}

Rules:
- Return at most ` + fmt.Sprintf("%d", req.MaxResults) + ` suggestions per version.
- Rank by confidence (highest first).
- confidence must be a number between 0.0 and 1.0.
- Use real ICD codes only; do not invent codes.
- Do NOT include any text outside the JSON object.`

	userMsg := "Clinical descriptions:\n" + strings.Join(req.Descriptions, "\n")

	resp, err := e.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: e.cfg.Model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userMsg},
		},
		Temperature: 0.2,
	})
	if err != nil {
		return nil, fmt.Errorf("LLM API error: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("LLM returned no choices")
	}

	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	// Strip potential markdown code fences
	raw = stripCodeFences(raw)

	var parsed struct {
		ICD9  []ICDSuggestion `json:"icd9_suggestions"`
		ICD10 []ICDSuggestion `json:"icd10_suggestions"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		log.Printf("llm: ERROR parse model=%q elapsed=%s err=%v", e.cfg.Model, time.Since(start).Round(time.Millisecond), err)
		return nil, fmt.Errorf("failed to parse LLM response JSON: %w", err)
	}

	// Clamp to MaxResults
	parsed.ICD9 = takeN(parsed.ICD9, req.MaxResults)
	parsed.ICD10 = takeN(parsed.ICD10, req.MaxResults)

	log.Printf("llm: done model=%q icd9=%d icd10=%d elapsed=%s",
		e.cfg.Model, len(parsed.ICD9), len(parsed.ICD10), time.Since(start).Round(time.Millisecond))

	return &InferResponse{
		Descriptions: req.Descriptions,
		ICD9:          parsed.ICD9,
		ICD10:         parsed.ICD10,
		Mode:          "llm",
		Model:         e.cfg.Model,
	}, nil
}

// heuristicInfer uses the keyword search store when no LLM is configured.
func (e *Engine) heuristicInfer(req InferRequest) (*InferResponse, error) {
	start := time.Now()
	log.Printf("llm [heuristic]: keyword search descriptions=%d max_results=%d", len(req.Descriptions), req.MaxResults)
	combined := strings.Join(req.Descriptions, " ")
	icd9Results, icd10Results := e.store.SearchAll(combined)

	toSuggestions := func(results []icd.SearchResult, n int) []ICDSuggestion {
		if len(results) > n {
			results = results[:n]
		}
		// Normalise scores to 0-1 relative to the top score.
		var maxScore float64
		for _, r := range results {
			if r.Score > maxScore {
				maxScore = r.Score
			}
		}
		out := make([]ICDSuggestion, len(results))
		for i, r := range results {
			conf := 0.0
			if maxScore > 0 {
				conf = r.Score / maxScore
			}
			out[i] = ICDSuggestion{
				Code:        r.Code,
				Description: r.Description,
				Category:    r.Category,
				Confidence:  round2(conf),
				Rationale:   "Matched by keyword similarity against local ICD database.",
			}
		}
		return out
	}

	icd9Sugg  := toSuggestions(icd9Results, req.MaxResults)
	icd10Sugg := toSuggestions(icd10Results, req.MaxResults)
	log.Printf("llm [heuristic]: done icd9=%d icd10=%d elapsed=%s",
		len(icd9Sugg), len(icd10Sugg), time.Since(start).Round(time.Millisecond))
	return &InferResponse{
		Descriptions: req.Descriptions,
		ICD9:          icd9Sugg,
		ICD10:         icd10Sugg,
		Mode:          "heuristic",
	}, nil
}

func takeN[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}

func stripCodeFences(s string) string {
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// ExpandQuery uses the LLM to generate alternative ICD-terminology-aligned
// phrasings of a clinical query. This bridges the vocabulary gap between
// free-text clinical descriptions and the formal ICD code descriptions.
//
// It returns 2-3 reformulations in the same language as the input query.
// If no LLM is configured, it returns nil without error (caller should use
// the original query only).
func (e *Engine) ExpandQuery(ctx context.Context, query string) ([]string, error) {
	if e.client == nil {
		return nil, nil
	}
	timeout := time.Duration(e.cfg.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	systemPrompt := `Sei un esperto di codifica ICD clinica.
Dato un testo clinico in input, genera fino a 3 riformulazioni alternative usando la terminologia medica ICD ufficiale italiana.
Le riformulazioni devono catturare lo stesso concetto clinico con parole diverse per migliorare il recupero semantico.
Rispondi SOLO con una lista di riformulazioni, una per riga, senza numerazione né testo aggiuntivo.`

	resp, err := e.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: e.cfg.Model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: query},
		},
		Temperature: 0.3,
		MaxTokens:   120,
	})
	if err != nil {
		return nil, fmt.Errorf("ExpandQuery LLM error: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, nil
	}
	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	var expansions []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		// Strip leading bullet/dash/number markers
		line = strings.TrimLeft(line, "-•*123456789. ")
		line = strings.TrimSpace(line)
		if line != "" && line != query {
			expansions = append(expansions, line)
		}
	}
	return expansions, nil
}
