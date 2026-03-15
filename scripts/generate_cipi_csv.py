#!/usr/bin/env python3
"""
generate_cipi_csv.py
Scarica il file ufficiale CIPI v.GAMMA 2.0 dal sito del Ministero della Salute
e genera data/official/cipi_codes.csv.

Uso:
    python scripts/generate_cipi_csv.py [--output data/official/cipi_codes.csv]

Fonte: https://www.salute.gov.it/new/it/tema/assistenza-ospedaliera-sdo/documentazione-tecnica/
File:  6.TAB_CIPI_V.GAMMA_2.0.xlsx
"""

import argparse
import csv
import io
import os
import sys
import urllib.request
import zipfile
import xml.etree.ElementTree as ET

CIPI_URL = (
    "https://www.salute.gov.it/new/sites/default/files/2026-02/"
    "6.TAB_CIPI_V.GAMMA_2.0.xlsx"
)

HEADERS = {
    "User-Agent": (
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
        "(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
    ),
    "Referer": "https://www.salute.gov.it/",
    "Accept": "application/octet-stream,*/*",
}

TIPO_MAP = {
    "2 Cifre, Rubrica": "chapter",
    "3 Cifre, Rubrica": "block",
    "4 Cifre, Rubrica": "category",
    "Codificante": "procedura",
}


def download_xlsx(url: str) -> bytes:
    req = urllib.request.Request(url, headers=HEADERS)
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = resp.read()
    if data[:2] in (b"PK", b"\x50\x4b"):
        return data
    # server returned HTML (redirect/auth page)
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


def row_to_sparse_dict(row, shared: list[str], ns: dict) -> dict[int, str]:
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


def get_parent(code: str) -> str:
    """Derive the hierarchical parent code from a CIPI code.

    Examples:
        "00"       → ""
        "00.0"     → "00"
        "00.00"    → "00.0"
        "00.00.0A" → "00.00"
    """
    parts = code.split(".")
    if len(parts) == 1:
        return ""
    if len(parts) == 2:
        prefix, suffix = parts
        if len(suffix) == 1:
            return prefix
        return prefix + "." + suffix[:-1]
    return ".".join(parts[:-1])


def parse_xlsx(data: bytes) -> list[dict]:
    z = zipfile.ZipFile(io.BytesIO(data))
    shared = load_shared_strings(z)

    ns = {"w": "http://schemas.openxmlformats.org/spreadsheetml/2006/main"}

    with z.open("xl/worksheets/sheet1.xml") as f:
        tree = ET.parse(f)

    rows = tree.findall(".//w:row", ns)

    in_data = False
    records = []
    for row in rows:
        cells = row_to_sparse_dict(row, shared, ns)
        # header row contains "CODICE CIPI 2025"
        if any("CODICE CIPI" in str(v) for v in cells.values()):
            in_data = True
            continue
        if not in_data:
            continue

        def col(i: int) -> str:
            return cells.get(i, "").strip()

        # column indices (0-based):
        #   0 = Codice identificativo (internal UUID-like ID)
        #   1 = Sezione
        #   2 = Tipo  (2 Cifre,Rubrica | 3 Cifre,Rubrica | 4 Cifre,Rubrica | Codificante)
        #   3 = Descrizione
        #   4 = CODICE CIPI 2025  ← the actual code
        code = col(4)
        tipo = col(2)
        desc = col(3)
        if not code or not desc:
            continue

        records.append(
            {
                "code": code,
                "description": desc,
                "type": TIPO_MAP.get(tipo, "procedura"),
                "parent": get_parent(code),
                "is_billable": 1 if tipo == "Codificante" else 0,
            }
        )

    return records


def write_csv(records: list[dict], path: str) -> None:
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    with open(path, "w", newline="", encoding="utf-8") as f:
        writer = csv.DictWriter(
            f, fieldnames=["code", "description", "type", "parent", "is_billable"]
        )
        writer.writeheader()
        writer.writerows(records)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        default="data/official/cipi_codes.csv",
        help="Output CSV path (default: data/official/cipi_codes.csv)",
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
        print(f"Downloading CIPI xlsx from:\n  {CIPI_URL}")
        data = download_xlsx(CIPI_URL)
        print(f"  Downloaded {len(data):,} bytes")

    print("Parsing xlsx…")
    records = parse_xlsx(data)

    billable = sum(1 for r in records if r["is_billable"])
    print(
        f"  Found {len(records):,} codes "
        f"({billable:,} billable + {len(records)-billable:,} rubrics)"
    )

    write_csv(records, args.output)
    print(f"Written: {args.output}")


if __name__ == "__main__":
    main()
