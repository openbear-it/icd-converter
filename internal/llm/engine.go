package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	// Use streaming so the TCP connection stays alive as long as tokens arrive;
	// the context deadline is the only hard limit (no response at all → timeout).
	stream, err := e.client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
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
	defer stream.Close()

	var rawBuf strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Printf("llm: stream error model=%q elapsed=%s err=%v", e.cfg.Model, time.Since(start).Round(time.Millisecond), err)
			return nil, fmt.Errorf("LLM stream error: %w", err)
		}
		if len(chunk.Choices) > 0 {
			rawBuf.WriteString(chunk.Choices[0].Delta.Content)
		}
	}

	raw := strings.TrimSpace(rawBuf.String())
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

// isICDCodePrefix reports whether s starts with an ICD code pattern:
// 1–2 uppercase ASCII letters immediately followed by a digit
// (e.g. "I11.0", "G45", "A01.2", "Z80.3 - ...")
func isICDCodePrefix(s string) bool {
	if len(s) < 2 {
		return false
	}
	letters := 0
	for _, c := range s {
		if c >= 'A' && c <= 'Z' {
			letters++
		} else {
			break
		}
	}
	if letters == 0 || letters > 2 {
		return false
	}
	if len(s) <= letters {
		return false
	}
	next := rune(s[letters])
	return next >= '0' && next <= '9'
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
	// No internal timeout — the caller provides the deadline via ctx.
	// Streaming keeps the TCP connection alive as long as tokens arrive;
	// the only hard limit is complete silence (ctx deadline exceeded).
	start := time.Now()

	// Truncate the query: expansion only needs key clinical concepts.
	// Long free-text causes reasoning models to spend all tokens on thinking.
	expandQuery := query
	const maxQueryRunes = 250
	if runes := []rune(query); len(runes) > maxQueryRunes {
		// Truncate at last space within limit to avoid cutting mid-word
		truncated := string(runes[:maxQueryRunes])
		if idx := strings.LastIndex(truncated, " "); idx > 0 {
			truncated = truncated[:idx]
		}
		expandQuery = truncated + "…"
	}

	systemPrompt := `Sei un codificatore ICD esperto. Identifica la diagnosi principale nel testo clinico e scrivi SOLO 2-3 sinonimi diagnostici italiani, uno per riga.
Regole ASSOLUTE:
- NIENTE codici ICD (niente lettere seguite da numeri come I10, G45, Z80)
- NIENTE trattini seguiti da codici
- Solo frasi diagnostiche brevi in italiano
- Niente titoli, asterischi, numeri, punteggiatura finale
Esempio output corretto:
ictus ischemico acuto
infarto cerebrale da cardioembolia
accidente cerebrovascolare ischemico`

	log.Printf("llm/expand: calling model=%q query=%q", e.cfg.Model, expandQuery)

	// Streaming — connection stays alive while the model generates tokens.
	stream, err := e.client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
		Model: e.cfg.Model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: expandQuery},
		},
		Temperature: 0.3,
		MaxTokens:   100,
	})
	if err != nil {
		log.Printf("llm/expand: ERROR model=%q elapsed=%s err=%v", e.cfg.Model, time.Since(start).Round(time.Millisecond), err)
		return nil, fmt.Errorf("ExpandQuery LLM error: %w", err)
	}
	defer stream.Close()

	var contentBuf strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Printf("llm/expand: stream error model=%q elapsed=%s err=%v", e.cfg.Model, time.Since(start).Round(time.Millisecond), err)
			return nil, fmt.Errorf("ExpandQuery LLM error: %w", err)
		}
		if len(chunk.Choices) > 0 {
			contentBuf.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	content := contentBuf.String()
	// Strip <think>...</think> blocks produced by reasoning models (e.g. qwen3)
	if idx := strings.LastIndex(content, "</think>"); idx != -1 {
		content = content[idx+len("</think>"):]
	}
	raw := strings.TrimSpace(content)
	log.Printf("llm/expand: response model=%q elapsed=%s raw=%q", e.cfg.Model, time.Since(start).Round(time.Millisecond), raw)
	var expansions []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		// Strip leading bullet/dash/number markers
		line = strings.TrimLeft(line, "-•*123456789. ")
		// Strip markdown bold/italic markers and trailing punctuation anywhere in line
		line = strings.ReplaceAll(line, "**", "")
		line = strings.ReplaceAll(line, "*", "")
		line = strings.TrimRight(line, ":;., ")
		line = strings.TrimSpace(line)
		// Skip empty, too short, or lines containing digit-heavy patterns
		// (hallucinated codes like "00-00-0000-..." or ICD code strings)
		if line == "" || line == query || len([]rune(line)) < 5 {
			continue
		}
		// Reject lines with 4+ consecutive digit groups (hallucinated code patterns)
		digitGroups := 0
		for _, part := range strings.Fields(line) {
			allDigitOrDash := true
			for _, c := range part {
				if (c < '0' || c > '9') && c != '-' {
					allDigitOrDash = false
					break
				}
			}
			if allDigitOrDash && len(part) > 2 {
				digitGroups++
			}
		}
		if digitGroups >= 2 {
			continue
		}
		// Reject lines that start with an ICD code pattern:
		// 1-2 uppercase letters followed immediately by digits (e.g. I11.0, G45, A01.2)
		if isICDCodePrefix(line) {
			continue
		}
		// Strip a trailing " - <description>" that follows a code prefix the
		// model placed mid-line (e.g. after a bullet was already stripped).
		if idx := strings.Index(line, " - "); idx > 0 {
			candidate := strings.TrimSpace(line[idx+3:])
			if !isICDCodePrefix(candidate) && len([]rune(candidate)) >= 5 {
				line = candidate
			} else {
				continue
			}
		}
		expansions = append(expansions, line)
	}
	log.Printf("llm/expand: done expansions=%d %v", len(expansions), expansions)
	return expansions, nil
}
