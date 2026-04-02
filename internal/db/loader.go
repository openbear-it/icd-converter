package db

import (
	"database/sql"
	"fmt"
	"strings"

	"icd-converter/internal/icd"
)

// LoadStore queries the active version from the database and returns a populated icd.Store.
func LoadStore(sqldb *sql.DB) (*icd.Store, error) {
	// Determine active version id
	var versionID int64
	err := sqldb.QueryRow(`SELECT id FROM icd_versions WHERE is_active=1 ORDER BY id DESC LIMIT 1`).Scan(&versionID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no active ICD version in database")
	}
	if err != nil {
		return nil, fmt.Errorf("query version: %w", err)
	}

	// ── Load mappings: icd9 → []icd10 and icd10 → []icd9 ─────────────────
	icd9to10 := make(map[string][]string)
	icd10to9 := make(map[string][]string)

	rows, err := sqldb.Query(
		`SELECT icd9_code, icd10_code FROM icd_mappings WHERE version_id = ?`, versionID)
	if err != nil {
		return nil, fmt.Errorf("query mappings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c9, c10 string
		if err := rows.Scan(&c9, &c10); err != nil {
			continue
		}
		icd9to10[c9] = append(icd9to10[c9], c10)
		icd10to9[c10] = append(icd10to9[c10], c9)
	}
	rows.Close()

	// ── Load ICD-9-CM diagnosi ─────────────────────────────────────────────
	icd9entries, err := loadICD9(sqldb, versionID, "icd9_diagnosi", icd9to10)
	if err != nil {
		return nil, err
	}

	// ── Load ICD-9-CM procedure ────────────────────────────────────────────
	procedureEntries, err := loadICD9(sqldb, versionID, "icd9_procedure", icd9to10)
	if err != nil {
		return nil, err
	}
	// Merge procedures into the ICD-9 list (they share the code space)
	icd9entries = append(icd9entries, procedureEntries...)

	// ── Load ICD-10-IM ─────────────────────────────────────────────────────
	icd10entries, err := loadICD10(sqldb, versionID, icd10to9)
	if err != nil {
		return nil, err
	}

	// ── Load CIPI ──────────────────────────────────────────────────────────
	cipiEntries, err := loadCIPI(sqldb, versionID)
	if err != nil {
		return nil, err
	}

	// ── Load DRG and MDC (global, not versioned) ───────────────────────────
	drgEntries, err := loadDRG(sqldb)
	if err != nil {
		return nil, err
	}
	mdcEntries, err := loadMDC(sqldb)
	if err != nil {
		return nil, err
	}

	return icd.NewStore(icd9entries, icd10entries, cipiEntries, drgEntries, mdcEntries), nil
}

func loadICD9(db *sql.DB, versionID int64, table string, icd9to10 map[string][]string) ([]icd.ICDEntry, error) {
	rows, err := db.Query(
		fmt.Sprintf(`SELECT code, description FROM %s WHERE version_id = ? ORDER BY code`, table),
		versionID)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", table, err)
	}
	defer rows.Close()

	type raw struct{ code, desc string }
	var raws []raw
	descByCode := make(map[string]string)
	for rows.Next() {
		var code, desc string
		if err := rows.Scan(&code, &desc); err != nil {
			continue
		}
		raws = append(raws, raw{code, desc})
		descByCode[code] = desc
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	entries := make([]icd.ICDEntry, 0, len(raws))
	for _, r := range raws {
		desc := r.desc
		if dot := strings.LastIndex(r.code, "."); dot > 0 {
			parentCode := r.code[:dot]
			if parentDesc, ok := descByCode[parentCode]; ok {
				desc = parentDesc + ": " + r.desc
			}
		}
		mappings := icd9to10[r.code]
		if mappings == nil {
			mappings = []string{}
		}
		entries = append(entries, icd.ICDEntry{
			Code:        r.code,
			Description: desc,
			Category:    icd9Category(r.code),
			Mappings:    mappings,
		})
	}
	return entries, nil
}

func loadICD10(db *sql.DB, versionID int64, icd10to9 map[string][]string) ([]icd.ICDEntry, error) {
	rows, err := db.Query(
		`SELECT code, description, type, parent FROM icd10_codes WHERE version_id = ? ORDER BY code`,
		versionID)
	if err != nil {
		return nil, fmt.Errorf("query icd10_codes: %w", err)
	}
	defer rows.Close()

	var entries []icd.ICDEntry
	for rows.Next() {
		var code, desc, typ, parent string
		if err := rows.Scan(&code, &desc, &typ, &parent); err != nil {
			continue
		}
		mappings := icd10to9[code]
		if mappings == nil {
			mappings = []string{}
		}
		// Use parent as category; fall back to type
		cat := parent
		if cat == "" {
			cat = typ
		}
		entries = append(entries, icd.ICDEntry{
			Code:        code,
			Description: desc,
			Category:    cat,
			Mappings:    mappings,
		})
	}
	return entries, rows.Err()
}

// icd9Category maps an ICD-9-CM code to its chapter name using the official Italian chapter ranges.
func icd9Category(code string) string {
	if len(code) == 0 {
		return ""
	}
	// V codes and E codes are special
	if code[0] == 'V' {
		return "Fattori influenzanti lo stato di salute"
	}
	if code[0] == 'E' {
		return "Cause esterne di traumatismo"
	}
	// Extract numeric prefix
	numStr := code
	if idx := indexOf(numStr, '.'); idx >= 0 {
		numStr = numStr[:idx]
	}
	var n int
	fmt.Sscanf(numStr, "%d", &n)

	switch {
	case n >= 1 && n <= 139:
		return "Malattie infettive e parassitarie"
	case n >= 140 && n <= 239:
		return "Tumori"
	case n >= 240 && n <= 279:
		return "Malattie delle ghiandole endocrine, nutrizione e metabolismo"
	case n >= 280 && n <= 289:
		return "Malattie del sangue e degli organi ematopoietici"
	case n >= 290 && n <= 319:
		return "Disturbi mentali"
	case n >= 320 && n <= 389:
		return "Malattie del sistema nervoso e degli organi di senso"
	case n >= 390 && n <= 459:
		return "Malattie del sistema circolatorio"
	case n >= 460 && n <= 519:
		return "Malattie dell'apparato respiratorio"
	case n >= 520 && n <= 579:
		return "Malattie dell'apparato digerente"
	case n >= 580 && n <= 629:
		return "Malattie dell'apparato genitourinario"
	case n >= 630 && n <= 679:
		return "Complicazioni della gravidanza, del parto e del puerperio"
	case n >= 680 && n <= 709:
		return "Malattie della pelle e del tessuto sottocutaneo"
	case n >= 710 && n <= 739:
		return "Malattie del sistema osteomuscolare e del tessuto connettivo"
	case n >= 740 && n <= 759:
		return "Malformazioni congenite"
	case n >= 760 && n <= 779:
		return "Alcune condizioni morbose di origine perinatale"
	case n >= 780 && n <= 799:
		return "Sintomi, segni e stati morbosi mal definiti"
	case n >= 800 && n <= 999:
		return "Traumatismi e avvelenamenti"
	default:
		return "Procedure ed interventi"
	}
}

func indexOf(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func loadCIPI(db *sql.DB, versionID int64) ([]icd.CIPIEntry, error) {
	rows, err := db.Query(
		`SELECT code, description, type, COALESCE(parent,'') FROM cipi_codes WHERE version_id = ? ORDER BY code`,
		versionID)
	if err != nil {
		return nil, fmt.Errorf("query cipi_codes: %w", err)
	}
	defer rows.Close()

	var entries []icd.CIPIEntry
	for rows.Next() {
		var code, desc, typ, parent string
		if err := rows.Scan(&code, &desc, &typ, &parent); err != nil {
			continue
		}
		entries = append(entries, icd.CIPIEntry{
			Code:        code,
			Description: desc,
			Type:        typ,
			Parent:      parent,
		})
	}
	return entries, rows.Err()
}

func loadDRG(db *sql.DB) ([]icd.DRGEntry, error) {
	rows, err := db.Query(
		`SELECT code, mdc, type, description, weight, geometric_los, arithmetic_los FROM drg_codes ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("query drg_codes: %w", err)
	}
	defer rows.Close()

	var entries []icd.DRGEntry
	for rows.Next() {
		var e icd.DRGEntry
		if err := rows.Scan(&e.Code, &e.MDC, &e.Type, &e.Description, &e.Weight, &e.GeometricLOS, &e.ArithmeticLOS); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func loadMDC(db *sql.DB) ([]icd.MDCEntry, error) {
	rows, err := db.Query(`SELECT code, description FROM mdc_codes ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("query mdc_codes: %w", err)
	}
	defer rows.Close()

	var entries []icd.MDCEntry
	for rows.Next() {
		var e icd.MDCEntry
		if err := rows.Scan(&e.Code, &e.Description); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

