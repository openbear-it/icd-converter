#!/usr/bin/env python3
"""
generate_cipi_mappings_csv.py
Scarica la tabella ufficiale di transcodifica ICD-9-CM ↔ CIPI v.GAMMA 2.0.1
dal sito del Ministero della Salute e genera data/official/cipi_mappings.csv.

Uso:
    python scripts/generate_cipi_mappings_csv.py [--output data/official/cipi_mappings.csv]

Fonte: https://www.salute.gov.it/new/it/tema/assistenza-ospedaliera-sdo/documentazione-tecnica/
File:  TRANSCODIFICA-ICD9CM-CIPI-V.2.0.1-WEB.xlsx

Struttura XLSX (foglio "TRANSCODIFICA_V.2.0.1"):
  Righe 1–4: titolo, licenza, istruzioni, riga vuota (saltate)
  Riga 5:    intestazione colonne
  Righe 6+:  dati

Colonne usate (0-indexed):
  1  = Codifica ICD9CM   → icd9_code  (può contenere codici extra tra parentesi, es. "00.24 (88.55)")
  2  = CODICE CIPI 2025  → cipi_code

Il CSV di output ha due colonne: icd9_code, cipi_code
Una riga per ogni coppia. I codici ICD-9-CM tra parentesi vengono ignorati
(sono codici secondari di accompagnamento, non il codice primario di riferimento).
"""

import argparse
import csv
import io
import os
import re
import sys
import urllib.request

CIPI_MAPPINGS_URL = (
    "https://www.salute.gov.it/new/sites/default/files/2026-03/"
    "TRANSCODIFICA-ICD9CM-CIPI-V.2.0.1-WEB.xlsx"
)

HEADERS = {
    "User-Agent": (
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
        "(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
    ),
    "Referer": "https://www.salute.gov.it/",
    "Accept": "application/octet-stream,*/*",
}

# Row index (0-based) where the actual data starts (after header at index 4)
DATA_START_ROW = 5


def download_xlsx(url: str) -> bytes:
    req = urllib.request.Request(url, headers=HEADERS)
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = resp.read()
    if data[:2] == b"PK":
        return data
    raise RuntimeError(
        f"Download failed — il server ha restituito contenuto non-XLSX "
        f"(primi byte: {data[:20]!r}). "
        "Provare a scaricare manualmente da " + url
    )


def parse_icd9_code(raw: str) -> str:
    """Estrae il codice ICD-9-CM primario, rimuovendo eventuali codici
    secondari indicati tra parentesi (es. '00.24 (88.55)' → '00.01')."""
    # Prende solo la parte prima delle parentesi
    code = re.split(r"\s*\(", raw)[0].strip()
    return code


def parse_xlsx(data: bytes) -> list[tuple[str, str]]:
    """Parsa il foglio TRANSCODIFICA_V.2.0.1 e restituisce coppie (icd9_code, cipi_code)."""
    try:
        import openpyxl
    except ImportError:
        print("Installo openpyxl…", file=sys.stderr)
        import subprocess
        subprocess.check_call([sys.executable, "-m", "pip", "install", "openpyxl", "-q"])
        import openpyxl

    wb = openpyxl.load_workbook(io.BytesIO(data), read_only=True, data_only=True)

    sheet_name = "TRANSCODIFICA_V.2.0.1"
    if sheet_name not in wb.sheetnames:
        raise RuntimeError(
            f"Foglio '{sheet_name}' non trovato. Fogli disponibili: {wb.sheetnames}"
        )
    ws = wb[sheet_name]

    pairs: list[tuple[str, str]] = []
    seen: set[tuple[str, str]] = set()

    for i, row in enumerate(ws.iter_rows(values_only=True)):
        if i < DATA_START_ROW:
            continue

        icd9_raw = row[1]
        cipi_raw = row[2]

        if not icd9_raw or not cipi_raw:
            continue

        icd9_code = parse_icd9_code(str(icd9_raw).strip())
        cipi_code = str(cipi_raw).strip()

        if not icd9_code or not cipi_code:
            continue

        key = (icd9_code, cipi_code)
        if key in seen:
            continue
        seen.add(key)
        pairs.append(key)

    return pairs


def write_csv(pairs: list[tuple[str, str]], output_path: str) -> int:
    os.makedirs(os.path.dirname(output_path), exist_ok=True)
    with open(output_path, "w", newline="", encoding="utf-8") as f:
        writer = csv.writer(f)
        writer.writerow(["icd9_code", "cipi_code"])
        for icd9_code, cipi_code in sorted(pairs):
            writer.writerow([icd9_code, cipi_code])
    return len(pairs)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        default="data/official/cipi_mappings.csv",
        help="Percorso del file CSV di output (default: data/official/cipi_mappings.csv)",
    )
    parser.add_argument(
        "--url",
        default=CIPI_MAPPINGS_URL,
        help="URL del file XLSX (default: URL ufficiale Ministero della Salute)",
    )
    args = parser.parse_args()

    print(f"Scarico {args.url} …", file=sys.stderr)
    data = download_xlsx(args.url)
    print(f"Scaricati {len(data):,} byte", file=sys.stderr)

    print("Parso il foglio TRANSCODIFICA_V.2.0.1 …", file=sys.stderr)
    pairs = parse_xlsx(data)
    print(f"Trovate {len(pairs):,} coppie univoche ICD-9 → CIPI", file=sys.stderr)

    n = write_csv(pairs, args.output)
    print(f"Scritto {args.output} con {n:,} righe", file=sys.stderr)


if __name__ == "__main__":
    main()
