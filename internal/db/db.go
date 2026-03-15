// Package db manages the SQLite database for ICD code storage with version tracking.
package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS icd_versions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL,
    year        INTEGER NOT NULL,
    source_url  TEXT,
    imported_at TEXT    NOT NULL,
    is_active   INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS icd9_diagnosi (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id  INTEGER NOT NULL REFERENCES icd_versions(id),
    code        TEXT    NOT NULL,
    description TEXT    NOT NULL,
    UNIQUE(version_id, code)
);

CREATE TABLE IF NOT EXISTS icd9_procedure (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id  INTEGER NOT NULL REFERENCES icd_versions(id),
    code        TEXT    NOT NULL,
    description TEXT    NOT NULL,
    UNIQUE(version_id, code)
);

CREATE TABLE IF NOT EXISTS icd10_codes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id  INTEGER NOT NULL REFERENCES icd_versions(id),
    code        TEXT    NOT NULL,
    description TEXT    NOT NULL,
    type        TEXT    NOT NULL,
    parent      TEXT,
    is_billable INTEGER NOT NULL DEFAULT 0,
    icd9_equiv  TEXT,
    UNIQUE(version_id, code)
);

CREATE TABLE IF NOT EXISTS icd_mappings (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id  INTEGER NOT NULL REFERENCES icd_versions(id),
    icd9_code   TEXT    NOT NULL,
    icd10_code  TEXT    NOT NULL,
    UNIQUE(version_id, icd9_code, icd10_code)
);

CREATE TABLE IF NOT EXISTS cipi_codes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id  INTEGER NOT NULL REFERENCES icd_versions(id),
    code        TEXT    NOT NULL,
    description TEXT    NOT NULL,
    type        TEXT    NOT NULL,  -- 'diagnosi' | 'procedura'
    parent      TEXT,
    is_billable INTEGER NOT NULL DEFAULT 0,
    UNIQUE(version_id, code)
);

CREATE TABLE IF NOT EXISTS icd_embeddings (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id INTEGER NOT NULL,
    icd_type   TEXT    NOT NULL,  -- 'icd9' | 'icd10' | 'cipi'
    code       TEXT    NOT NULL,
    model      TEXT    NOT NULL,
    vector     BLOB    NOT NULL,  -- IEEE-754 float32 little-endian
    UNIQUE(version_id, icd_type, code, model)
);

CREATE INDEX IF NOT EXISTS idx_icd9d_code  ON icd9_diagnosi(code);
CREATE INDEX IF NOT EXISTS idx_icd9p_code  ON icd9_procedure(code);
CREATE INDEX IF NOT EXISTS idx_icd10_code  ON icd10_codes(code);
CREATE INDEX IF NOT EXISTS idx_cipi_code   ON cipi_codes(code);
CREATE INDEX IF NOT EXISTS idx_map_icd9    ON icd_mappings(icd9_code);
CREATE INDEX IF NOT EXISTS idx_map_icd10   ON icd_mappings(icd10_code);
CREATE INDEX IF NOT EXISTS idx_emb_lookup  ON icd_embeddings(version_id, icd_type, model);
`

// Open opens (or creates) the SQLite database at path and migrates the schema.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	return nil
}

// IsSeeded reports whether the database already contains at least one active version.
func IsSeeded(db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM icd_versions WHERE is_active = 1`).Scan(&count)
	return count > 0, err
}

// GetActiveVersionID returns the id of the most recent active ICD version.
func GetActiveVersionID(db *sql.DB) (int64, error) {
	var id int64
	err := db.QueryRow(`SELECT id FROM icd_versions WHERE is_active=1 ORDER BY id DESC LIMIT 1`).Scan(&id)
	return id, err
}
