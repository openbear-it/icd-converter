// Package embed provides vector-embedding-based semantic search over ICD codes.
// It uses any OpenAI-compatible embeddings endpoint (OpenAI, Ollama, LM Studio, etc.)
// and caches computed vectors in SQLite to avoid recomputing on every startup.
package embed

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"

	"icd-converter/internal/icd"

	openai "github.com/sashabaranov/go-openai"
)

// Config holds the configuration for the embedding engine.
type Config struct {
	// APIKey is the OpenAI (or compatible) API key.
	APIKey string
	// BaseURL overrides the default endpoint (e.g. "http://localhost:11434/v1" for Ollama).
	BaseURL string
	// Model is the embedding model name (e.g. "all-minilm", "text-embedding-3-small").
	Model string
	// BatchSize is the number of texts sent per API call (default 64).
	BatchSize int
	// EnrichText controls whether the indexed text is enriched with category and code
	// context before embedding. When true the index text becomes
	// "Description. Category" — this improves recall because the model has access to
	// chapter/block context in addition to the leaf description.
	// Enabling this uses a separate cache namespace ("_enriched" suffix) so it never
	// collides with vectors computed in plain mode.
	EnrichText bool
}

// SearchResult is an ICD entry paired with its cosine similarity score.
type SearchResult struct {
	icd.ICDEntry
	Score float64 `json:"score"`
}

// Index holds precomputed embedding vectors for a set of ICD entries and supports
// cosine-similarity nearest-neighbour search.
type Index struct {
	entries []icd.ICDEntry
	vecs    [][]float32
}

// Len returns the number of indexed entries.
func (idx *Index) Len() int { return len(idx.entries) }

// Search returns the top-N entries whose embedding is closest to query.
func (idx *Index) Search(query []float32, topN int) []SearchResult {
	type candidate struct {
		idx   int
		score float64
	}
	cands := make([]candidate, len(idx.entries))
	for i, v := range idx.vecs {
		cands[i] = candidate{i, cosine(query, v)}
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].score > cands[b].score })

	n := topN
	if n > len(cands) {
		n = len(cands)
	}
	out := make([]SearchResult, n)
	for i := 0; i < n; i++ {
		out[i] = SearchResult{ICDEntry: idx.entries[cands[i].idx], Score: cands[i].score}
	}
	return out
}

// MultiQuerySearch splits text into clauses, embeds each one independently, and
// merges results by summing cosine scores across queries (Reciprocal Score Fusion
// lite). It returns at most topN results sorted by combined score.
// queryVecs must be pre-computed by the caller (one vector per clause).
func (idx *Index) MultiQuerySearch(queryVecs [][]float32, topN int) []SearchResult {
	if len(queryVecs) == 0 {
		return nil
	}
	scores := make([]float64, len(idx.entries))
	for _, qv := range queryVecs {
		for i, v := range idx.vecs {
			scores[i] += cosine(qv, v)
		}
	}
	// Normalise by number of queries so score stays in [0,1]-ish range.
	n := float64(len(queryVecs))
	type candidate struct {
		idx   int
		score float64
	}
	cands := make([]candidate, len(idx.entries))
	for i, s := range scores {
		cands[i] = candidate{i, s / n}
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].score > cands[b].score })
	if topN > len(cands) {
		topN = len(cands)
	}
	out := make([]SearchResult, topN)
	for i := 0; i < topN; i++ {
		out[i] = SearchResult{ICDEntry: idx.entries[cands[i].idx], Score: cands[i].score}
	}
	return out
}

// SplitClinicalText splits a clinical text into meaningful clauses suitable for
// multi-query embedding. Sentences and comma-separated phrases longer than
// minLen characters are kept; very short fragments are discarded.
// If the text is short (≤ shortTextThreshold chars) it is returned as-is.
func SplitClinicalText(text string, minLen int) []string {
	const shortTextThreshold = 80
	text = strings.TrimSpace(text)
	if len(text) <= shortTextThreshold {
		return []string{text}
	}
	// Split on sentence-ending punctuation and semicolons first, then commas.
	raw := strings.FieldsFunc(text, func(r rune) bool {
		return r == '.' || r == ';' || r == '\n'
	})
	var chunks []string
	for _, part := range raw {
		part = strings.TrimSpace(part)
		if len(part) < minLen {
			continue
		}
		// Further split long comma-phrases to avoid diluting meaning.
		if len(part) > 120 {
			sub := strings.Split(part, ",")
			for _, s := range sub {
				s = strings.TrimSpace(s)
				if len(s) >= minLen {
					chunks = append(chunks, s)
				}
			}
		} else {
			chunks = append(chunks, part)
		}
	}
	if len(chunks) == 0 {
		return []string{text}
	}
	return chunks
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		na += ai * ai
		nb += bi * bi
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Builder computes embeddings via an OpenAI-compatible API and caches them in SQLite.
type Builder struct {
	cfg    Config
	client *openai.Client
}

// NewBuilder creates a Builder from the given config.
func NewBuilder(cfg Config) *Builder {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 64
	}
	oc := openai.DefaultConfig(cfg.APIKey)
	if cfg.BaseURL != "" {
		oc.BaseURL = cfg.BaseURL
	}
	return &Builder{cfg: cfg, client: openai.NewClientWithConfig(oc)}
}

// ModelName returns the configured embedding model name.
func (b *Builder) ModelName() string { return b.cfg.Model }

// EmbedOne returns the embedding vector for a single text.
func (b *Builder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vecs, err := b.embedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("empty embedding returned by API")
	}
	return vecs[0], nil
}

// BuildOrLoad loads a cached Index from SQLite if all vectors are present,
// otherwise computes them via the API and persists the result.
func (b *Builder) BuildOrLoad(ctx context.Context, sqldb *sql.DB, versionID int64, icdType string, entries []icd.ICDEntry) (*Index, error) {
	cacheType := icdType
	if b.cfg.EnrichText {
		cacheType = icdType + "_enriched"
	}
	vecs, err := loadFromDB(sqldb, versionID, cacheType, b.cfg.Model, entries)
	if err != nil {
		log.Printf("embed: db load error for %s (%v) — will recompute", cacheType, err)
	}
	if vecs != nil {
		log.Printf("embed: loaded %d cached vectors  type=%s  model=%s", len(vecs), cacheType, b.cfg.Model)
		return &Index{entries: entries, vecs: vecs}, nil
	}

	log.Printf("embed: computing %d vectors  type=%s  model=%s …", len(entries), cacheType, b.cfg.Model)
	vecs, err = b.computeFull(ctx, icdType, entries)
	if err != nil {
		return nil, err
	}
	if err := saveToDB(sqldb, versionID, cacheType, b.cfg.Model, entries, vecs); err != nil {
		log.Printf("embed: warning: could not cache vectors: %v", err)
	}
	return &Index{entries: entries, vecs: vecs}, nil
}

func (b *Builder) computeFull(ctx context.Context, icdType string, entries []icd.ICDEntry) ([][]float32, error) {
	// Build a code→description lookup so we can enrich leaf entries with their
	// parent category descriptions (e.g. ICD-10 leaf codes get their block label).
	catByCode := make(map[string]string, len(entries))
	for _, e := range entries {
		catByCode[e.Code] = e.Description
	}

	all := make([][]float32, len(entries))
	for start := 0; start < len(entries); start += b.cfg.BatchSize {
		end := start + b.cfg.BatchSize
		if end > len(entries) {
			end = len(entries)
		}
		texts := make([]string, end-start)
		for i, e := range entries[start:end] {
			texts[i] = entryText(e, catByCode, b.cfg.EnrichText)
		}
		vecs, err := b.embedBatch(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("embed batch [%d:%d]: %w", start, end, err)
		}
		copy(all[start:end], vecs)
		if end%5000 == 0 || end == len(entries) {
			log.Printf("embed: [%s] %d/%d (%d%%)", icdType, end, len(entries), end*100/len(entries))
		}
	}
	return all, nil
}

// entryText returns the text to embed for an ICD entry.
// When enrich is true the category is appended (looked up from catByCode when it
// looks like a code rather than a human-readable string).
func entryText(e icd.ICDEntry, catByCode map[string]string, enrich bool) string {
	if !enrich || e.Category == "" {
		return e.Description
	}
	// Resolve the category: if it matches a known code use its description,
	// otherwise use the raw category string (already human-readable for ICD-9 chapters).
	catDesc := e.Category
	if desc, ok := catByCode[e.Category]; ok {
		catDesc = desc
	}
	if catDesc == e.Description {
		return e.Description
	}
	return e.Description + ". " + catDesc
}

func (b *Builder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	req := openai.EmbeddingRequest{
		Input: texts,
		Model: openai.EmbeddingModel(b.cfg.Model),
	}
	resp, err := b.client.CreateEmbeddings(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("embeddings API: %w", err)
	}
	result := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index < len(result) {
			result[d.Index] = d.Embedding
		}
	}
	return result, nil
}

// ── SQLite persistence ───────────────────────────────────────────────────────

func saveToDB(db *sql.DB, versionID int64, icdType, model string, entries []icd.ICDEntry, vecs [][]float32) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO icd_embeddings(version_id,icd_type,code,model,vector) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, e := range entries {
		if _, err := stmt.Exec(versionID, icdType, e.Code, model, float32sToBlob(vecs[i])); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func loadFromDB(db *sql.DB, versionID int64, icdType, model string, entries []icd.ICDEntry) ([][]float32, error) {
	rows, err := db.Query(
		`SELECT code, vector FROM icd_embeddings WHERE version_id=? AND icd_type=? AND model=?`,
		versionID, icdType, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string][]float32, len(entries))
	for rows.Next() {
		var code string
		var blob []byte
		if err := rows.Scan(&code, &blob); err != nil {
			return nil, err
		}
		m[code] = blobToFloat32s(blob)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(m) < len(entries) {
		return nil, nil // incomplete cache — trigger recompute
	}

	vecs := make([][]float32, len(entries))
	for i, e := range entries {
		v, ok := m[e.Code]
		if !ok {
			return nil, nil // missing entry — trigger recompute
		}
		vecs[i] = v
	}
	return vecs, nil
}

// ── Binary encoding ──────────────────────────────────────────────────────────

func float32sToBlob(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func blobToFloat32s(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
