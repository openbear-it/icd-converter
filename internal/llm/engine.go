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
	// TimeoutSeconds is the per-request timeout for inference calls (default: 60).
	TimeoutSeconds int
	// ExpandTimeoutSeconds is the budget for non-critical query expansion (default: 10).
	// Keep this short: expansion is best-effort and must not block search results.
	ExpandTimeoutSeconds int
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
	if cfg.ExpandTimeoutSeconds <= 0 {
		cfg.ExpandTimeoutSeconds = 10
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

// ExpandTimeout returns the configured budget for best-effort query expansion.
func (e *Engine) ExpandTimeout() time.Duration {
	return time.Duration(e.cfg.ExpandTimeoutSeconds) * time.Second
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

// extractDiagnosisExcerpt builds a short focused excerpt from a (potentially
// long) clinical text to send to the LLM for query expansion.
//
// Strategy:
//  1. Split the text into sentences.
//  2. Collect sentences that contain explicit diagnosis markers
//     ("diagnosi", "eziologia", "riscontro di", "si tratta di", "compatibile con").
//  3. If none found, fall back to a head-truncated version of the original.
//
// The result is capped at 300 runes so the model focuses on key concepts.
func extractDiagnosisExcerpt(query string) string {
	const maxRunes = 300
	// Italian diagnosis-indicator keywords (lowercase for matching)
	markers := []string{
		"diagnosi", "eziologia", "riscontro di", "si tratta di",
		"compatibile con", "quadro di", "conferma di",
	}

	// Split on sentence-ending punctuation followed by space or newline.
	sentences := strings.FieldsFunc(query, func(r rune) bool {
		return r == '\n'
	})
	// Also split run-on sentences at ". " boundaries.
	var parts []string
	for _, s := range sentences {
		for _, p := range strings.Split(s, ". ") {
			p = strings.TrimSpace(p)
			if p != "" {
				parts = append(parts, p)
			}
		}
	}

	var diagParts []string
	for _, p := range parts {
		lower := strings.ToLower(p)
		for _, m := range markers {
			if strings.Contains(lower, m) {
				diagParts = append(diagParts, p)
				break
			}
		}
	}

	var excerpt string
	if len(diagParts) > 0 {
		excerpt = strings.Join(diagParts, ". ")
	} else {
		excerpt = query
	}

	// Cap at maxRunes.
	runes := []rune(excerpt)
	if len(runes) > maxRunes {
		truncated := string(runes[:maxRunes])
		if idx := strings.LastIndex(truncated, " "); idx > 0 {
			truncated = truncated[:idx]
		}
		excerpt = truncated + "…"
	}
	return excerpt
}
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

	// Build a focused excerpt for the LLM: prefer sentences that contain the
	// explicit diagnosis over truncating from the start (which cuts it off in
	// long admission notes where the diagnosis appears at the end).
	expandQuery := extractDiagnosisExcerpt(query)

	systemPrompt := `Sei un codificatore ICD esperto. Nel testo clinico è descritta una diagnosi principale. Scrivi SOLO 2-3 sinonimi ICD italiani di quella diagnosi, UNO PER RIGA, senza virgole tra le voci.
Regole ASSOLUTE:
- UNO SOLO per riga — NON usare virgole per separare più voci
- NIENTE codici ICD (niente lettere seguite da numeri come I10, G45, Z80, I21, I35)
- NIENTE sintomi (dolore, dispnea, sincope, ecc.) — solo la diagnosi finale
- NIENTE parametri clinici (score, frazione, gradiente, classe NYHA, ecc.)
- Solo frasi diagnostiche brevi in italiano
- Niente titoli, asterischi, numeri, punteggiatura finale
Esempio (testo: "...indicazione a TAVI per stenosi aortica severa..."):
stenosi aortica severa sintomatica
valvulopatia aortica ostruttiva critica
impianto transcatetere di valvola aortica`

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

	// Normalise separators: the model sometimes uses commas instead of newlines.
	// Replace " , " and "," with newlines so each candidate is on its own line.
	normalised := strings.NewReplacer(", ", "\n", ",", "\n").Replace(raw)

	var expansions []string
	for _, line := range strings.Split(normalised, "\n") {
		line = strings.TrimSpace(line)
		// Strip leading bullet/dash/number markers
		line = strings.TrimLeft(line, "-•*123456789. ")
		// Strip markdown bold/italic markers and trailing punctuation
		line = strings.ReplaceAll(line, "**", "")
		line = strings.ReplaceAll(line, "*", "")
		line = strings.TrimRight(line, ":;., ")
		line = strings.TrimSpace(line)

		if line == "" || line == query || len([]rune(line)) < 5 {
			continue
		}

		// Reject lines that start with an ICD code pattern (e.g. I11.0, G45)
		if isICDCodePrefix(line) {
			continue
		}

		// Strip " - description" suffix that follows a code prefix mid-line.
		if idx := strings.Index(line, " - "); idx > 0 {
			candidate := strings.TrimSpace(line[idx+3:])
			if !isICDCodePrefix(candidate) && len([]rune(candidate)) >= 5 {
				line = candidate
			} else {
				continue
			}
		}

		// Reject lines with 2+ numeric-only tokens (hallucinated code patterns)
		digitGroups := 0
		for _, part := range strings.Fields(line) {
			pureNum := true
			for _, c := range part {
				if (c < '0' || c > '9') && c != '-' {
					pureNum = false
					break
				}
			}
			if pureNum && len(part) > 2 {
				digitGroups++
			}
		}
		if digitGroups >= 2 {
			continue
		}

		// Reject non-diagnostic fragments: lines that are just clinical parameters
		// or symptom descriptions without a diagnostic noun.
		lower := strings.ToLower(line)
		nonDiagnosticTokens := []string{
			"score", "classe nyha", "nyha", "gradiente", "frazione di eiezione",
			"sincope", "dolore", "dispnea", "diaforesi", "febbre", "vomito",
			"nausea", "cefalea", "palpitazioni", "edema", "tosse",
		}
		skip := false
		for _, tok := range nonDiagnosticTokens {
			if strings.HasPrefix(lower, tok) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		expansions = append(expansions, line)
		if len(expansions) == 3 {
			break // hard cap at 3
		}
	}
	log.Printf("llm/expand: done expansions=%d %v", len(expansions), expansions)
	return expansions, nil
}
