#!/usr/bin/env python3
"""Translate DRG code descriptions from English to Italian using local Ollama."""

import csv, json, urllib.request, sys, time

OLLAMA_URL = "http://localhost:11434/api/chat"
MODEL = "gemma3:4b"
BATCH = 15
IN_CSV  = "data/official/drg_codes.csv"
OUT_CSV = "data/official/drg_codes.csv"

SYSTEM = (
    "Sei un traduttore medico esperto. "
    "Traduci le descrizioni di DRG (Diagnosis Related Groups) dall'inglese all'italiano. "
    "Mantieni le sigle tecniche originali: MCC, CC, MV, O.R., ECMO, CAR, AMI, AICD. "
    "Rispondi SOLO con una lista JSON di stringhe tradotte, nello stesso ordine dell'input. "
    "Non aggiungere commenti o spiegazioni."
)

def translate_batch(descriptions: list[str]) -> list[str]:
    prompt = "Traduci queste descrizioni DRG in italiano:\n" + json.dumps(descriptions, ensure_ascii=False)
    payload = json.dumps({
        "model": MODEL,
        "messages": [
            {"role": "system", "content": SYSTEM},
            {"role": "user",   "content": prompt},
        ],
        "stream": False,
    }).encode()
    req = urllib.request.Request(OLLAMA_URL, data=payload, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = json.loads(resp.read())
    text = data["message"]["content"].strip()
    # Extract JSON array from response
    start = text.find("[")
    end   = text.rfind("]") + 1
    if start == -1 or end == 0:
        raise ValueError(f"No JSON array in response: {text[:200]}")
    translations = json.loads(text[start:end])
    if len(translations) != len(descriptions):
        raise ValueError(
            f"Expected {len(descriptions)} translations, got {len(translations)}: {text[:200]}"
        )
    return [str(t) for t in translations]


def main():
    with open(IN_CSV, newline="", encoding="utf-8") as f:
        rows = list(csv.DictReader(f))
    fieldnames = list(rows[0].keys())

    total = len(rows)
    translated_rows = list(rows)  # copy

    for i in range(0, total, BATCH):
        batch_rows = rows[i : i + BATCH]
        descs = [r["description"] for r in batch_rows]
        attempt = 0
        while attempt < 3:
            try:
                it_descs = translate_batch(descs)
                for j, it_desc in enumerate(it_descs):
                    translated_rows[i + j]["description"] = it_desc
                print(f"  [{i+len(batch_rows)}/{total}] {descs[0][:60]} → {it_descs[0][:60]}", flush=True)
                break
            except Exception as e:
                attempt += 1
                print(f"  [RETRY {attempt}/3] batch {i}: {e}", file=sys.stderr)
                time.sleep(2)
        else:
            print(f"  [SKIP] batch {i}: kept English", file=sys.stderr)

    with open(OUT_CSV, "w", newline="", encoding="utf-8") as f:
        writer = csv.DictWriter(f, fieldnames=fieldnames)
        writer.writeheader()
        writer.writerows(translated_rows)
    print(f"\nDone. Written {total} rows to {OUT_CSV}")


if __name__ == "__main__":
    main()
