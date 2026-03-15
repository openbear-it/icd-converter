#!/usr/bin/env python3
"""
generate_icd10im_mappings_csv.py
Scarica la tabella ufficiale di transcodifica ICD-9-CM ↔ ICD-10-IM v.GAMMA 2.1
dal sito del Ministero della Salute e genera data/official/icd10im_mappings.csv.

Uso:
    python scripts/generate_icd10im_mappings_csv.py [--output data/official/icd10im_mappings.csv]

Fonte: https://www.salute.gov.it/new/it/tema/assistenza-ospedaliera-sdo/documentazione-tecnica/
File:  4.TAB_TRANSCOD_ICD-9-CM-ICD-10-IM_V.GAMMA_2.1.XLSX

Struttura XLSX:
  Le prime righe contengono titolo e note di licenza.
  Lo script individua automaticamente la riga di intestazione cercando
  celle che contengano "icd9" oppure "icd-9" (case-insensitive).
  Le colonne attese nell'intestazione sono:
    - Codice ICD-9-CM    → icd9_code
    - Descrizione ICD-9  → icd9_desc
    - Codice ICD-10-IM   → icd10_code
    - Descrizione ICD-10 → icd10_desc
"""

import argparse
import csv
import io
import os
import sys
import urllib.request
import zipfile
import xml.etree.ElementTree as ET

MAPPINGS_URL = (
    "https://www.salute.gov.it/new/sites/default/files/2026-02/"
    "4.TAB_TRANSCOD_ICD-9-CM-ICD-10-IM_V.GAMMA_2.1.XLSX"
)

HEADERS = {
    "User-Agent": (
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
        "(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
    ),
    "Referer": "https://www.salute.gov.it/",
    "Accept": "application/octet-stream,*/*",
}


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


def get_cell_value(c, shared: list[str], ns: dict) -> str:
    t = c.get("t", "")
    v_el = c.find("w:v", ns)
    v = v_el.text if v_el is not None else ""
    if t == "s" and v:
        return shared[int(v)]
    return v or ""


def find_column_indices(header_cells: list[str]) -> dict[str, int]:
    """Find column indices by matching header keywords."""
    col = {}
    lowered = [c.lower() for c in header_cells]
    for i, h in enumerate(lowered):
        if "icd-9" in h or "icd9" in h:
            if "desc" in h or "titolo" in h or "diagnosi" in h:
                col.setdefault("icd9_desc", i)
            else:
                col.setdefault("icd9_code", i)
        elif "icd-10" in h or "icd10" in h:
            if "desc" in h or "titolo" in h:
                col.setdefault("icd10_desc", i)
            else:
                col.setdefault("icd10_code", i)
    return col


def parse_xlsx(data: bytes) -> list[dict]:
    z = zipfile.ZipFile(io.BytesIO(data))
    shared = load_shared_strings(z)
    ns = {"w": "http://schemas.openxmlformats.org/spreadsheetml/2006/main"}

    with z.open("xl/worksheets/sheet1.xml") as f:
        tree = ET.parse(f)

    rows = tree.findall(".//w:row", ns)
    records = []

    header_row_idx = None
    col_map: dict[str, int] = {}

    for row_idx, row in enumerate(rows):
        cells = [get_cell_value(c, shared, ns) for c in row.findall("w:c", ns)]
        if not cells:
            continue

        if header_row_idx is None:
            # Detect header row: must contain ICD-9 and ICD-10 references
            lowered = " ".join(cells).lower()
            if ("icd-9" in lowered or "icd9" in lowered) and (
                "icd-10" in lowered or "icd10" in lowered
            ):
                col_map = find_column_indices(cells)
                header_row_idx = row_idx
                if len(col_map) < 2:
                    # fallback: assume fixed column order icd9_code, icd9_desc, icd10_code, icd10_desc
                    col_map = {"icd9_code": 0, "icd9_desc": 1, "icd10_code": 2, "icd10_desc": 3}
                continue
        else:
            def col(key: str) -> str:
                i = col_map.get(key, -1)
                return cells[i].strip() if 0 <= i < len(cells) else ""

            icd9_code = col("icd9_code")
            icd10_code = col("icd10_code")
            if not icd9_code and not icd10_code:
                continue

            records.append(
                {
                    "icd9_code": icd9_code,
                    "icd9_desc": col("icd9_desc"),
                    "icd10_code": icd10_code,
                    "icd10_desc": col("icd10_desc"),
                }
            )

    return records


def write_csv(records: list[dict], path: str) -> None:
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    with open(path, "w", newline="", encoding="utf-8") as f:
        writer = csv.DictWriter(
            f, fieldnames=["icd9_code", "icd9_desc", "icd10_code", "icd10_desc"]
        )
        writer.writeheader()
        writer.writerows(records)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        default="data/official/icd10im_mappings.csv",
        help="Output CSV path (default: data/official/icd10im_mappings.csv)",
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
        print(f"Downloading mappings xlsx from:\n  {MAPPINGS_URL}")
        data = download_xlsx(MAPPINGS_URL)
        print(f"  Downloaded {len(data):,} bytes")

    print("Parsing xlsx…")
    records = parse_xlsx(data)
    print(f"  Found {len(records):,} mappings")

    write_csv(records, args.output)
    print(f"Written: {args.output}")


if __name__ == "__main__":
    main()
