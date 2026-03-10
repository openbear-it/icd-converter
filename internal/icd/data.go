package icd

// ICDEntry represents a single ICD code entry with its description and cross-references.
type ICDEntry struct {
	Code        string   `json:"code"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Mappings    []string `json:"mappings"` // corresponding codes in the other version
}
