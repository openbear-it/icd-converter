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
