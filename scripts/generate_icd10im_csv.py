#!/usr/bin/env python3
"""
generate_icd10im_csv.py
Scarica il file ufficiale Elenco Sistematico ICD-10-IM v.GAMMA 2.1 dal sito del
Ministero della Salute e genera data/official/icd10im_codes.csv.

Uso:
    python scripts/generate_icd10im_csv.py [--output data/official/icd10im_codes.csv]

Fonte: https://www.salute.gov.it/new/it/tema/assistenza-ospedaliera-sdo/documentazione-tecnica/
File:  3.ELENCO_SISTEMATICO_ICD-10-IM_V.GAMMA_2.1_VALIDITA_DA_16022026.xlsx

Struttura XLSX (versione GAMMA 2.1, Dicembre 2025):
  Riga 1: titolo
  Riga 2: nota di licenza (CC BY-NC-ND 4.0)
  Riga 3: intestazione colonne
  Righe 4+: dati

Colonne usate (0-indexed):
  1  = Codice             → code
  2  = Titolo             → description
  3  = Tipo               → type  (chapter | block | cat 3 | cat 4)
  4  = Padre              → parent
  6  = Codice Terminale   → is_billable  (0 = no, 1 = sì)
  16 = Transcodifica ICD-9-CM → icd9_equiv
"""

import argparse
import csv
import io
import os
import sys
import urllib.request
import zipfile
import xml.etree.ElementTree as ET

ICD10IM_URL = (
    "https://www.salute.gov.it/new/sites/default/files/2026-02/"
    "3.ELENCO_SISTEMATICO_ICD-10-IM_V.GAMMA_2.1_VALIDITA_DA_16022026.xlsx"
)

HEADERS = {
    "User-Agent": (
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
        "(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
    ),
    "Referer": "https://www.salute.gov.it/",
    "Accept": "application/octet-stream,*/*",
}

# XLSX column indices (0-based) in the data rows
COL_CODE = 1
COL_DESC = 2
COL_TYPE = 3
COL_PARENT = 4
COL_BILLABLE = 6
COL_ICD9 = 16
# Number of header rows to skip before data starts
HEADER_ROWS = 3


def download_xlsx(url: str) -> bytes:
    req = urllib.request.Request(url, headers=HEADERS)
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = resp.read()
    if data[:2] == b"PK":
        return data
    raise RuntimeError(
        f"Download failed — server returned non-XLSX content "
        f"(first bytes: {data[:20]!r}). "
        "Try downloading manually from " + url
    )


def load_shared_strings(z: zipfile.ZipFile) -> list[str]:
    shared = []
    try:
        with z.open("xl/sharedStrings.xml") as f:
            tree = ET.parse(f)
            ns = {"w": "http://schemas.openxmlformats.org/spreadsheetml/2006/main"}
            for si in tree.findall(".//w:si", ns):
                t = "".join(
                    e.text or ""
                    for e in si.iter(
                        "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}t"
                    )
                )
                shared.append(t)
    except KeyError:
        pass
    return shared


def col_letter_to_index(col_str: str) -> int:
    """Convert xlsx column letter to 0-based index: 'A'→0, 'B'→1, 'AA'→26."""
    result = 0
    for ch in col_str.upper():
        result = result * 26 + (ord(ch) - ord("A") + 1)
    return result - 1


def row_to_sparse_list(row, shared: list[str], ns: dict) -> dict[int, str]:
    """Return {col_index: value} for a row, respecting actual column positions."""
    result = {}
    for c in row.findall("w:c", ns):
        ref = c.get("r", "")
        col_letters = "".join(ch for ch in ref if ch.isalpha())
        idx = col_letter_to_index(col_letters) if col_letters else len(result)
        t = c.get("t", "")
        v_el = c.find("w:v", ns)
        v = v_el.text if v_el is not None else ""
        result[idx] = shared[int(v)] if t == "s" and v else v or ""
    return result


def parse_xlsx(data: bytes) -> list[dict]:
    z = zipfile.ZipFile(io.BytesIO(data))
    shared = load_shared_strings(z)
    ns = {"w": "http://schemas.openxmlformats.org/spreadsheetml/2006/main"}

    with z.open("xl/worksheets/sheet1.xml") as f:
        tree = ET.parse(f)

    rows = tree.findall(".//w:row", ns)
    records = []

    for row_idx, row in enumerate(rows):
        if row_idx < HEADER_ROWS:
            continue  # skip title, license, column-header rows

        cells = row_to_sparse_list(row, shared, ns)

        def col(i: int) -> str:
            return cells.get(i, "").strip()

        code = col(COL_CODE)
        desc = col(COL_DESC)
        if not code or not desc:
            continue

        records.append(
            {
                "code": code,
                "description": desc,
                "type": col(COL_TYPE),
                "parent": col(COL_PARENT),
                "is_billable": col(COL_BILLABLE) or "0",
                "icd9_equiv": col(COL_ICD9),
            }
        )

    return records


def write_csv(records: list[dict], path: str) -> None:
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    with open(path, "w", newline="", encoding="utf-8") as f:
        writer = csv.DictWriter(
            f,
            fieldnames=["code", "description", "type", "parent", "is_billable", "icd9_equiv"],
        )
        writer.writeheader()
        writer.writerows(records)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        default="data/official/icd10im_codes.csv",
        help="Output CSV path (default: data/official/icd10im_codes.csv)",
    )
    parser.add_argument(
        "--input",
        default=None,
        help="Local XLSX file to use instead of downloading",
    )
    args = parser.parse_args()

    if args.input:
        print(f"Reading local file: {args.input}")
        with open(args.input, "rb") as f:
            data = f.read()
    else:
        print(f"Downloading ICD-10-IM xlsx from:\n  {ICD10IM_URL}")
        data = download_xlsx(ICD10IM_URL)
        print(f"  Downloaded {len(data):,} bytes")

    print("Parsing xlsx…")
    records = parse_xlsx(data)
    billable = sum(1 for r in records if r["is_billable"] == "1")
    print(
        f"  Found {len(records):,} codes "
        f"({billable:,} billable + {len(records)-billable:,} non-billable)"
    )

    write_csv(records, args.output)
    print(f"Written: {args.output}")


if __name__ == "__main__":
    main()
