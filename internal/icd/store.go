package icd

import (
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
}

// NewStore builds a Store from pre-loaded slices (e.g. loaded from SQLite).
func NewStore(icd9 []ICDEntry, icd10 []ICDEntry, cipi []CIPIEntry) *Store {
	s := &Store{
		icd9ByCode:  make(map[string]ICDEntry, len(icd9)),
		icd10ByCode: make(map[string]ICDEntry, len(icd10)),
		icd9List:    icd9,
		icd10List:   icd10,
		cipiByCode:  make(map[string]CIPIEntry, len(cipi)),
		cipiList:    cipi,
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

// SearchCIPI performs a case-insensitive keyword search over CIPI descriptions.
// An optional cipiType ("diagnosi" | "procedura") restricts the results; pass ""
// to search all types.
func (s *Store) SearchCIPI(query, cipiType string) []CIPISearchResult {
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil
	}
	var results []CIPISearchResult
	for _, e := range s.cipiList {
		if cipiType != "" && e.Type != cipiType {
			continue
		}
		desc := strings.ToLower(e.Description)
		score := scoreEntry(desc, strings.ToLower(e.Code), tokens)
		if score > 0 {
			results = append(results, CIPISearchResult{CIPIEntry: e, Score: score})
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

// searchEntries scores entries against a multi-word query.
// Scoring: +2 per token that is a prefix of a description word, +1 per token contained anywhere.
func searchEntries(entries []ICDEntry, query string) []SearchResult {
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil
	}

	var results []SearchResult
	for _, e := range entries {
		desc := strings.ToLower(e.Description)
		score := scoreEntry(desc, strings.ToLower(e.Code), tokens)
		if score > 0 {
			results = append(results, SearchResult{ICDEntry: e, Score: score})
		}
	}

	// Sort descending by score (simple insertion sort – data set is small).
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].Score > results[j-1].Score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
	return results
}

func scoreEntry(desc, code string, tokens []string) float64 {
	var score float64
	for _, tok := range tokens {
		if strings.Contains(code, tok) {
			score += 3
		}
		if strings.Contains(desc, tok) {
			score += 2
		}
		// Partial word prefix bonus
		for _, word := range strings.Fields(desc) {
			if strings.HasPrefix(word, tok) {
				score += 1
			}
		}
	}
	return score
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
