package icd

import (
	"math"
	"strings"
)

// Store is the in-memory ICD code store with bidirectional lookup.
type Store struct {
	icd9ByCode  map[string]ICDEntry
	icd10ByCode map[string]ICDEntry
	icd9List    []ICDEntry
	icd10List   []ICDEntry
	cipiByCode  map[string]CIPIEntry
	cipiList    []CIPIEntry
	drgByCode   map[string]DRGEntry
	drgList     []DRGEntry
	mdcByCode   map[string]MDCEntry
	mdcList     []MDCEntry
}

// NewStore builds a Store from pre-loaded slices (e.g. loaded from SQLite).
func NewStore(icd9 []ICDEntry, icd10 []ICDEntry, cipi []CIPIEntry, drg []DRGEntry, mdc []MDCEntry) *Store {
	s := &Store{
		icd9ByCode:  make(map[string]ICDEntry, len(icd9)),
		icd10ByCode: make(map[string]ICDEntry, len(icd10)),
		icd9List:    icd9,
		icd10List:   icd10,
		cipiByCode:  make(map[string]CIPIEntry, len(cipi)),
		cipiList:    cipi,
		drgByCode:   make(map[string]DRGEntry, len(drg)),
		drgList:     drg,
		mdcByCode:   make(map[string]MDCEntry, len(mdc)),
		mdcList:     mdc,
	}
	for _, e := range icd9 {
		s.icd9ByCode[e.Code] = e
	}
	for _, e := range icd10 {
		s.icd10ByCode[e.Code] = e
	}
	for _, e := range cipi {
		s.cipiByCode[e.Code] = e
	}
	for _, e := range drg {
		s.drgByCode[e.Code] = e
	}
	for _, e := range mdc {
		s.mdcByCode[e.Code] = e
	}
	return s
}

// LookupICD9 returns the ICD-9 entry for a given code (case-insensitive, trimmed).
func (s *Store) LookupICD9(code string) (ICDEntry, bool) {
	e, ok := s.icd9ByCode[strings.TrimSpace(code)]
	return e, ok
}

// LookupICD10 returns the ICD-10 entry for a given code.
func (s *Store) LookupICD10(code string) (ICDEntry, bool) {
	e, ok := s.icd10ByCode[strings.TrimSpace(code)]
	return e, ok
}

// ICD9ToICD10 converts an ICD-9 code to the mapped ICD-10 entries.
func (s *Store) ICD9ToICD10(code string) ([]ICDEntry, bool) {
	src, ok := s.icd9ByCode[strings.TrimSpace(code)]
	if !ok {
		return nil, false
	}
	results := make([]ICDEntry, 0, len(src.Mappings))
	for _, m := range src.Mappings {
		if e, found := s.icd10ByCode[m]; found {
			results = append(results, e)
		}
	}
	return results, true
}

// ICD10ToICD9 converts an ICD-10 code to the mapped ICD-9 entries.
func (s *Store) ICD10ToICD9(code string) ([]ICDEntry, bool) {
	src, ok := s.icd10ByCode[strings.TrimSpace(code)]
	if !ok {
		return nil, false
	}
	results := make([]ICDEntry, 0, len(src.Mappings))
	for _, m := range src.Mappings {
		if e, found := s.icd9ByCode[m]; found {
			results = append(results, e)
		}
	}
	return results, true
}

// SearchResult is a code entry with a relevance score.
type SearchResult struct {
	ICDEntry
	Score float64 `json:"score"`
}

// SearchICD9 performs a case-insensitive keyword search over ICD-9 descriptions.
func (s *Store) SearchICD9(query string) []SearchResult {
	return searchEntries(s.icd9List, query)
}

// SearchICD10 performs a case-insensitive keyword search over ICD-10 descriptions.
func (s *Store) SearchICD10(query string) []SearchResult {
	return searchEntries(s.icd10List, query)
}

// SearchAll performs a combined keyword search over both ICD-9 and ICD-10 entries.
func (s *Store) SearchAll(query string) (icd9Results, icd10Results []SearchResult) {
	return s.SearchICD9(query), s.SearchICD10(query)
}

// AllICD9 returns all ICD-9 entries.
func (s *Store) AllICD9() []ICDEntry { return s.icd9List }

// AllICD10 returns all ICD-10 entries.
func (s *Store) AllICD10() []ICDEntry { return s.icd10List }

// AllCIPI returns all CIPI entries.
func (s *Store) AllCIPI() []CIPIEntry { return s.cipiList }

// AllDRG returns all DRG entries.
func (s *Store) AllDRG() []DRGEntry { return s.drgList }

// AllMDC returns all MDC entries.
func (s *Store) AllMDC() []MDCEntry { return s.mdcList }

// LookupDRG returns the DRG entry for a given code (trimmed, zero-padded to 3 digits).
func (s *Store) LookupDRG(code string) (DRGEntry, bool) {
	code = normalizeDRGCode(code)
	e, ok := s.drgByCode[code]
	return e, ok
}

// LookupMDC returns the MDC entry for a given code.
func (s *Store) LookupMDC(code string) (MDCEntry, bool) {
	e, ok := s.mdcByCode[strings.TrimSpace(code)]
	return e, ok
}

// DRGsByMDCAndType returns all DRG entries for a given MDC and type ("SURG" or "MED").
func (s *Store) DRGsByMDCAndType(mdc, drgType string) []DRGEntry {
	var out []DRGEntry
	for _, e := range s.drgList {
		if e.MDC == mdc && e.Type == drgType {
			out = append(out, e)
		}
	}
	return out
}

// DRGSearchResult is a DRG entry paired with a relevance score.
type DRGSearchResult struct {
	DRGEntry
	Score float64 `json:"score"`
}

// SearchDRG performs a BM25 keyword search over DRG descriptions.
// If mdc is non-empty only DRGs in that MDC are searched.
func (s *Store) SearchDRG(query, mdc string) []DRGSearchResult {
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil
	}
	return bm25DRG(s.drgList, tokens, mdc)
}

// normalizeDRGCode zero-pads a DRG code to 3 characters (e.g. "1" → "001").
func normalizeDRGCode(code string) string {
	code = strings.TrimSpace(code)
	for len(code) < 3 {
		code = "0" + code
	}
	return code
}

// LookupCIPI returns the CIPI entry for a given code (case-insensitive, trimmed).
func (s *Store) LookupCIPI(code string) (CIPIEntry, bool) {
	e, ok := s.cipiByCode[strings.TrimSpace(code)]
	return e, ok
}

// ChildrenCIPI returns all CIPI entries whose parent equals the given code.
func (s *Store) ChildrenCIPI(code string) []CIPIEntry {
	code = strings.TrimSpace(code)
	var children []CIPIEntry
	for _, e := range s.cipiList {
		if e.Parent == code {
			children = append(children, e)
		}
	}
	if children == nil {
		children = []CIPIEntry{}
	}
	return children
}

// CIPISearchResult is a CIPI entry with a relevance score.
type CIPISearchResult struct {
	CIPIEntry
	Score float64 `json:"score"`
}

// SearchCIPI performs a BM25 keyword search over CIPI descriptions.
// An optional cipiType ("diagnosi" | "procedura") restricts the results; pass ""
// to search all types.
func (s *Store) SearchCIPI(query, cipiType string) []CIPISearchResult {
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil
	}
	return bm25CIPI(s.cipiList, tokens, cipiType)
}

// ChildrenICD9 returns all ICD-9 entries whose code is a direct or indirect
// child of parentCode (i.e. starts with "parentCode.").
func (s *Store) ChildrenICD9(parentCode string) []ICDEntry {
	return childrenOf(s.icd9List, strings.TrimSpace(parentCode))
}

// ChildrenICD10 returns all ICD-10 entries whose code is a direct or indirect
// child of parentCode.
func (s *Store) ChildrenICD10(parentCode string) []ICDEntry {
	return childrenOf(s.icd10List, strings.TrimSpace(parentCode))
}

// childrenOf filters a list returning all entries whose code starts with prefix+"."
func childrenOf(list []ICDEntry, prefix string) []ICDEntry {
	results := make([]ICDEntry, 0)
	dotPrefix := prefix + "."
	for _, e := range list {
		if strings.HasPrefix(e.Code, dotPrefix) {
			results = append(results, e)
		}
	}
	return results
}

// searchEntries scores entries against a multi-word query using BM25.
// The corpus statistics (avgdl, idf) are computed on the fly from the given slice.
// A small code-match bonus is added on top so that exact code searches rank first.
func searchEntries(entries []ICDEntry, query string) []SearchResult {
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil
	}

	// Build per-document term frequency maps and compute avgdl.
	type docFields struct {
		descTokens []string
		tf         map[string]int
	}
	docs := make([]docFields, len(entries))
	var totalLen int
	for i, e := range entries {
		words := tokenize(e.Description)
		tf := make(map[string]int, len(words))
		for _, w := range words {
			tf[w]++
		}
		docs[i] = docFields{descTokens: words, tf: tf}
		totalLen += len(words)
	}
	N := len(entries)
	avgdl := 1.0
	if N > 0 {
		avgdl = float64(totalLen) / float64(N)
	}

	// BM25 parameters (standard tuning).
	const k1 = 1.5
	const b = 0.75

	// Document frequency per query token.
	df := make(map[string]int, len(tokens))
	for _, tok := range tokens {
		for _, d := range docs {
			if d.tf[tok] > 0 {
				df[tok]++
			}
		}
	}

	// Score each document.
	var results []SearchResult
	for i, e := range entries {
		d := docs[i]
		dl := float64(len(d.descTokens))
		var score float64
		for _, tok := range tokens {
			tfVal := float64(d.tf[tok])
			if tfVal == 0 {
				continue
			}
			idf := math.Log((float64(N)-float64(df[tok])+0.5)/(float64(df[tok])+0.5) + 1)
			score += idf * (tfVal * (k1 + 1)) / (tfVal + k1*(1-b+b*dl/avgdl))
		}
		// Code-match bonus: exact or prefix match on the code string.
		codeLower := strings.ToLower(e.Code)
		for _, tok := range tokens {
			if strings.HasPrefix(codeLower, tok) {
				score += 5
			}
		}
		if score > 0 {
			results = append(results, SearchResult{ICDEntry: e, Score: score})
		}
	}

	// Sort descending by score.
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].Score > results[j-1].Score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
	return results
}

// bm25CIPI is the same BM25 logic applied to CIPIEntry slices.
func bm25CIPI(entries []CIPIEntry, tokens []string, cipiType string) []CIPISearchResult {
	type docFields struct {
		words []string
		tf    map[string]int
	}
	// Filter by type first so IDF is computed on the relevant subset.
	var subset []CIPIEntry
	for _, e := range entries {
		if cipiType == "" || e.Type == cipiType {
			subset = append(subset, e)
		}
	}
	if len(subset) == 0 {
		return nil
	}

	docs := make([]docFields, len(subset))
	var totalLen int
	for i, e := range subset {
		words := tokenize(e.Description)
		tf := make(map[string]int, len(words))
		for _, w := range words {
			tf[w]++
		}
		docs[i] = docFields{words: words, tf: tf}
		totalLen += len(words)
	}
	N := len(subset)
	avgdl := float64(totalLen) / float64(N)

	const k1 = 1.5
	const bParam = 0.75

	df := make(map[string]int, len(tokens))
	for _, tok := range tokens {
		for _, d := range docs {
			if d.tf[tok] > 0 {
				df[tok]++
			}
		}
	}

	var results []CIPISearchResult
	for i, e := range subset {
		d := docs[i]
		dl := float64(len(d.words))
		var score float64
		for _, tok := range tokens {
			tfVal := float64(d.tf[tok])
			if tfVal == 0 {
				continue
			}
			idf := math.Log((float64(N)-float64(df[tok])+0.5)/(float64(df[tok])+0.5) + 1)
			score += idf * (tfVal * (k1 + 1)) / (tfVal + k1*(1-bParam+bParam*dl/avgdl))
		}
		codeLower := strings.ToLower(e.Code)
		for _, tok := range tokens {
			if strings.HasPrefix(codeLower, tok) {
				score += 5
			}
		}
		if score > 0 {
			results = append(results, CIPISearchResult{CIPIEntry: e, Score: score})
		}
	}
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].Score > results[j-1].Score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
	return results
}

func tokenize(s string) []string {
	lower := strings.ToLower(s)
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return r == ' ' || r == ',' || r == ';' || r == '.' || r == '-' || r == '/'
	})
	var tokens []string
	for _, f := range fields {
		if len(f) >= 2 { // ignore single-char tokens
			tokens = append(tokens, f)
		}
	}
	return tokens
}

// bm25DRG is the BM25 search logic for DRGEntry slices, optionally filtered by MDC.
func bm25DRG(entries []DRGEntry, tokens []string, mdc string) []DRGSearchResult {
	type docFields struct {
		words []string
		tf    map[string]int
	}
	var subset []DRGEntry
	for _, e := range entries {
		if mdc == "" || strings.EqualFold(e.MDC, mdc) {
			subset = append(subset, e)
		}
	}
	if len(subset) == 0 {
		return nil
	}

	docs := make([]docFields, len(subset))
	var totalLen int
	for i, e := range subset {
		words := tokenize(e.Description)
		tf := make(map[string]int, len(words))
		for _, w := range words {
			tf[w]++
		}
		docs[i] = docFields{words: words, tf: tf}
		totalLen += len(words)
	}
	N := len(subset)
	avgdl := float64(totalLen) / float64(N)
	const k1, bParam = 1.5, 0.75

	df := make(map[string]int, len(tokens))
	for _, tok := range tokens {
		for _, d := range docs {
			if d.tf[tok] > 0 {
				df[tok]++
			}
		}
	}

	var results []DRGSearchResult
	for i, e := range subset {
		d := docs[i]
		dl := float64(len(d.words))
		var score float64
		for _, tok := range tokens {
			tfVal := float64(d.tf[tok])
			if tfVal == 0 {
				continue
			}
			idf := math.Log((float64(N)-float64(df[tok])+0.5)/(float64(df[tok])+0.5) + 1)
			score += idf * (tfVal * (k1 + 1)) / (tfVal + k1*(1-bParam+bParam*dl/avgdl))
		}
		// bonus for code match
		for _, tok := range tokens {
			if strings.HasPrefix(strings.ToLower(e.Code), tok) {
				score += 5
			}
		}
		if score > 0 {
			results = append(results, DRGSearchResult{DRGEntry: e, Score: score})
		}
	}
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].Score > results[j-1].Score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
	return results
}

