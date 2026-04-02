package icd

// ICDEntry represents a single ICD code entry with its description and cross-references.
type ICDEntry struct {
	Code        string   `json:"code"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Mappings    []string `json:"mappings"` // corresponding codes in the other version
}

// CIPIEntry represents a single CIPI code entry.
type CIPIEntry struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Type        string `json:"type"`   // "diagnosi" | "procedura"
	Parent      string `json:"parent"` // parent code in the hierarchy
}

// DRGEntry represents a single CMS MS-DRG entry (FY2026 v43.0).
type DRGEntry struct {
	Code          string  `json:"code"`
	MDC           string  `json:"mdc"`           // "01"–"25" or "PRE"
	Type          string  `json:"type"`          // "SURG" | "MED"
	Description   string  `json:"description"`
	Weight        float64 `json:"weight"`
	GeometricLOS  float64 `json:"geometric_los"`
	ArithmeticLOS float64 `json:"arithmetic_los"`
}

// MDCEntry represents a Major Diagnostic Category.
type MDCEntry struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}
