package db

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"io/fs"
	"log"
	"strings"
	"time"
)

// SeedOptions configures a seeding run.
type SeedOptions struct {
	VersionName string
	Year        int
	SourceURL   string
	DataFS      fs.FS // FS rooted at the directory containing official/*.csv
}

// Seed imports all official ICD data from embedded CSVs into the database.
// It is idempotent: if a version with the same name already exists the seed is skipped.
func Seed(db *sql.DB, opts SeedOptions) error {
	// Check if this version already exists
	var existing int
	_ = db.QueryRow(`SELECT COUNT(*) FROM icd_versions WHERE name = ?`, opts.VersionName).Scan(&existing)
	if existing > 0 {
		log.Printf("db: version %q already seeded, skipping", opts.VersionName)
		return nil
	}

	log.Printf("db: seeding version %q …", opts.VersionName)
	start := time.Now()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Insert version record
	res, err := tx.Exec(
		`INSERT INTO icd_versions(name, year, source_url, imported_at, is_active) VALUES(?,?,?,?,1)`,
		opts.VersionName, opts.Year, opts.SourceURL, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("insert version: %w", err)
	}
	versionID, _ := res.LastInsertId()

	// ── ICD-9-CM diagnosi ──────────────────────────────────────────────────
	n9d, err := importCSV(tx, opts.DataFS, "data/official/diagnosi_icd9cm.csv",
		`INSERT OR IGNORE INTO icd9_diagnosi(version_id, code, description) VALUES(?,?,?)`,
		versionID, 2)
	if err != nil {
		return fmt.Errorf("icd9 diagnosi: %w", err)
	}

	// ── ICD-9-CM procedure ─────────────────────────────────────────────────
	n9p, err := importCSV(tx, opts.DataFS, "data/official/procedure_icd9cm.csv",
		`INSERT OR IGNORE INTO icd9_procedure(version_id, code, description) VALUES(?,?,?)`,
		versionID, 2)
	if err != nil {
		return fmt.Errorf("icd9 procedure: %w", err)
	}

	// ── ICD-10-IM codes ────────────────────────────────────────────────────
	n10, err := importICD10(tx, opts.DataFS, versionID)
	if err != nil {
		return fmt.Errorf("icd10 codes: %w", err)
	}

	// ── ICD-10-IM → ICD-9-CM mappings ─────────────────────────────────────
	nm, err := importMappings(tx, opts.DataFS, versionID)
	if err != nil {
		return fmt.Errorf("mappings: %w", err)
	}

	// ── CIPI codes ─────────────────────────────────────────────────────────
	ncipi, err := importCIPI(tx, opts.DataFS, versionID)
	if err != nil {
		return fmt.Errorf("cipi codes: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	log.Printf("db: seeded in %s — ICD-9 diagnosi: %d, procedure: %d, ICD-10: %d, mappings: %d, CIPI: %d",
		time.Since(start).Round(time.Millisecond), n9d, n9p, n10, nm, ncipi)
	return nil
}

// importCSV reads a 2-column CSV (code, description, header on row 0) and bulk-inserts rows.
func importCSV(tx *sql.Tx, fsys fs.FS, path, stmt string, versionID int64, cols int) (int, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	_, _ = r.Read() // skip header

	prep, err := tx.Prepare(stmt)
	if err != nil {
		return 0, err
	}
	defer prep.Close()

	var count int
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(rec) < cols {
			continue
		}
		code := strings.TrimSpace(rec[0])
		desc := strings.TrimSpace(rec[1])
		if code == "" || desc == "" {
			continue
		}
		if _, err := prep.Exec(versionID, code, desc); err != nil {
			continue
		}
		count++
	}
	return count, nil
}

// importICD10 reads icd10im_codes.csv and inserts all ICD-10-IM entries.
// CSV columns: code, description, type, parent, is_billable, icd9_equiv
func importICD10(tx *sql.Tx, fsys fs.FS, versionID int64) (int, error) {
	f, err := fsys.Open("data/official/icd10im_codes.csv")
	if err != nil {
		return 0, fmt.Errorf("open icd10im_codes.csv: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	_, _ = r.Read() // skip header

	prep, err := tx.Prepare(
		`INSERT OR IGNORE INTO icd10_codes(version_id, code, description, type, parent, is_billable, icd9_equiv)
		 VALUES(?,?,?,?,?,?,?)`,
	)
	if err != nil {
		return 0, err
	}
	defer prep.Close()

	var count int
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(rec) < 2 {
			continue
		}
		code := strings.TrimSpace(rec[0])
		desc := strings.TrimSpace(rec[1])
		if code == "" || desc == "" {
			continue
		}
		typ := ""
		parent := ""
		billable := "0"
		icd9eq := ""
		if len(rec) > 2 {
			typ = strings.TrimSpace(rec[2])
		}
		if len(rec) > 3 {
			parent = strings.TrimSpace(rec[3])
		}
		if len(rec) > 4 {
			billable = strings.TrimSpace(rec[4])
		}
		if len(rec) > 5 {
			icd9eq = strings.TrimSpace(rec[5])
		}
		b := 0
		if billable == "1" {
			b = 1
		}
		if _, err := prep.Exec(versionID, code, desc, typ, parent, b, icd9eq); err != nil {
			continue
		}
		count++
	}
	return count, nil
}

// importMappings reads icd10im_mappings.csv and inserts ICD-9 → ICD-10 mapping rows.
// CSV columns: icd9_code, icd9_desc, icd10_code, icd10_desc
func importMappings(tx *sql.Tx, fsys fs.FS, versionID int64) (int, error) {
	f, err := fsys.Open("data/official/icd10im_mappings.csv")
	if err != nil {
		return 0, fmt.Errorf("open icd10im_mappings.csv: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	_, _ = r.Read() // skip header

	prep, err := tx.Prepare(
		`INSERT OR IGNORE INTO icd_mappings(version_id, icd9_code, icd10_code) VALUES(?,?,?)`,
	)
	if err != nil {
		return 0, err
	}
	defer prep.Close()

	var count int
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(rec) < 3 {
			continue
		}
		count++
	}
	return count, nil
}

// importCIPI reads cipi_codes.csv and inserts all CIPI entries.
// CSV columns: code, description, type, parent, is_billable
func importCIPI(tx *sql.Tx, fsys fs.FS, versionID int64) (int, error) {
	f, err := fsys.Open("data/official/cipi_codes.csv")
	if err != nil {
		return 0, fmt.Errorf("open cipi_codes.csv: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	_, _ = r.Read() // skip header

	prep, err := tx.Prepare(
		`INSERT OR IGNORE INTO cipi_codes(version_id, code, description, type, parent, is_billable)
		 VALUES(?,?,?,?,?,?)`,
	)
	if err != nil {
		return 0, err
	}
	defer prep.Close()

	var count int
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(rec) < 2 {
			continue
		}
		code := strings.TrimSpace(rec[0])
		desc := strings.TrimSpace(rec[1])
		if code == "" || desc == "" {
			continue
		}
		typ := ""
		parent := ""
		billable := "0"
		if len(rec) > 2 {
			typ = strings.TrimSpace(rec[2])
		}
		if len(rec) > 3 {
			parent = strings.TrimSpace(rec[3])
		}
		if len(rec) > 4 {
			billable = strings.TrimSpace(rec[4])
		}
		b := 0
		if billable == "1" {
			b = 1
		}
		if _, err := prep.Exec(versionID, code, desc, typ, parent, b); err != nil {
			continue
		}
		count++
	}
	return count, nil
}
