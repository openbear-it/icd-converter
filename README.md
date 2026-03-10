# ICD Converter

Servizio REST in Go per la conversione tra codici **ICD-9-CM** e **ICD-10-IM**, con supporto a un **motore di inferenza** per suggerire codici a partire da descrizioni cliniche in linguaggio naturale.

I dati provengono dalle fonti ufficiali del **Ministero della Salute italiano**: 16.212 diagnosi ICD-9-CM, 4.460 procedure ICD-9-CM, 14.773 codici ICD-10-IM e 18.189 mappature di transcodifica (versione GAMMA 2.1, valida dal 16/02/2026), tutti incorporati nel binario via `go:embed`.

## Funzionalità

| Feature | Descrizione |
|---|---|
| Conversione ICD-9 → ICD-10 | Dato un codice ICD-9 restituisce i codici ICD-10 equivalenti |
| Conversione ICD-10 → ICD-9 | Dato un codice ICD-10 restituisce i codici ICD-9 equivalenti |
| Ricerca testuale | Cerca codici per parola chiave in entrambe le versioni |
| Inferenza | Analizza descrizioni cliniche e suggerisce i codici più pertinenti |
| Lista / Paginazione | Esplora i codici per versione e categoria con paginazione |
| UI Web | Pagina HTML per il testing immediato di tutte le funzionalità |

## Struttura del progetto

```
icd-converter/
├── main.go                    # Entry point, router, config da env
├── data/
│   └── official/              # CSV ufficiali incorporati nel binario (go:embed)
│       ├── diagnosi_icd9cm.csv    # 16.212 diagnosi ICD-9-CM
│       ├── procedure_icd9cm.csv   # 4.460 procedure ICD-9-CM
│       ├── icd10im_codes.csv      # 14.773 codici ICD-10-IM
│       └── icd10im_mappings.csv   # 18.189 mappature di transcodifica
├── internal/
│   ├── db/
│   │   ├── db.go              # Apertura SQLite e schema migration
│   │   ├── seed.go            # Seeder idempotente da CSV embedded
│   │   └── loader.go          # Carica icd.Store dal DB con categorie italiane
│   ├── icd/
│   │   ├── data.go            # Struttura ICDEntry
│   │   └── store.go           # Lookup, conversione e ricerca testuale
│   ├── api/
│   │   ├── handlers.go        # Handler REST (conversione, ricerca, lista)
│   │   └── infer.go           # Handler POST /infer (LLM)
│   └── llm/
│       └── engine.go          # Motore LLM (OpenAI-compatible + fallback euristico)
└── web/
    └── index.html             # UI di test (tab: Converti, Ricerca, LLM, Esplora, API, Fonti)
```

## Avvio rapido

```bash
# Build
go build -o icd-converter .

# Avvio (modalità euristica, senza LLM esterno)
./icd-converter

# Avvio con OpenAI
OPENAI_API_KEY=sk-... ./icd-converter

# Avvio con Ollama (o qualsiasi endpoint OpenAI-compatible)
LLM_BASE_URL=http://localhost:11434/v1 LLM_MODEL=llama3 ./icd-converter
```

Il server parte su `http://localhost:8080` (porta configurabile via `PORT=xxxx`).

## Docker

```bash
# Build immagine
docker build -t icd-converter .

# Avvio con volume persistente per il DB SQLite
docker run -d \
  -p 8080:8080 \
  -v icd_data:/data \
  -e ICD_DB_PATH=/data/icd.db \
  icd-converter

# Con LLM esterno
docker run -d \
  -p 8080:8080 \
  -v icd_data:/data \
  -e ICD_DB_PATH=/data/icd.db \
  -e OPENAI_API_KEY=sk-... \
  icd-converter
```

L'immagine multi-arch (`linux/amd64` + `linux/arm64`) è pubblicata automaticamente su `ghcr.io` ad ogni push su `main`.

## Variabili di ambiente

| Variabile | Default | Descrizione |
|---|---|---|
| `PORT` | `8080` | Porta HTTP |
| `OPENAI_API_KEY` | *(vuoto)* | Chiave API OpenAI. Se assente si usa la modalità euristica |
| `LLM_BASE_URL` | OpenAI default | Override URL base (Ollama, LM Studio, Azure…) |
| `LLM_MODEL` | `gpt-4o-mini` | Nome del modello da usare |
| `GIN_MODE` | `release` | `debug` per log verbose di Gin |
| `ICD_DB_PATH` | `icd.db` | Percorso del database SQLite (es. `/data/icd.db` per Docker) |

## API Reference

### Conversione

```
GET /api/v1/icd9/{code}              → lookup codice ICD-9
GET /api/v1/icd9/{code}/to-icd10     → converte ICD-9 → ICD-10
GET /api/v1/icd10/{code}             → lookup codice ICD-10
GET /api/v1/icd10/{code}/to-icd9     → converte ICD-10 → ICD-9
```

### Lista / Paginazione

```
GET /api/v1/icd9?page=1&limit=20&category=Malattie+del+sistema+circolatorio
GET /api/v1/icd10?page=1&limit=20&category=Tumori
```

Alcune categorie ICD-9 disponibili: `Malattie infettive e parassitarie`, `Tumori`, `Malattie del sistema circolatorio`, `Malattie dell'apparato respiratorio`, `Traumatismi e avvelenamenti`, `Procedure ed interventi`, … (capitoli ICD-9-CM standard).

### Ricerca testuale

```
GET /api/v1/search?q=diabetes&version=both
# version: "icd9" | "icd10" | "both"
```

### Inferenza LLM

```
POST /api/v1/infer
Content-Type: application/json

{
  "descriptions": [
    "Dolore toracico acuto con ST sopraslivellato",
    "Ipertensione arteriosa",
    "Diabete mellito tipo 2"
  ],
  "max_results": 5
}
```

Risposta:
```json
{
  "descriptions": [...],
  "icd9_suggestions": [
    { "code": "410.91", "description": "...", "category": "Malattie del sistema circolatorio", "confidence": 0.95, "rationale": "..." }
  ],
  "icd10_suggestions": [...],
  "mode": "llm",        // oppure "heuristic"
  "model": "gpt-4o-mini"
}
```

### Utilità

```
GET /health             → {"status":"ok"}
GET /ui/                → UI di test HTML
```

## Esempi curl

```bash
# Conversione ICD-9 → ICD-10
curl http://localhost:8080/api/v1/icd9/427.31/to-icd10

# Conversione ICD-10 → ICD-9
curl http://localhost:8080/api/v1/icd10/I21.9/to-icd9

# Ricerca per keyword
curl "http://localhost:8080/api/v1/search?q=fibrillazione+atriale"

# Inferenza LLM
curl -X POST http://localhost:8080/api/v1/infer \
  -H 'Content-Type: application/json' \
  -d '{"descriptions":["paziente con polmonite batterica e insufficienza respiratoria"],"max_results":5}'
```

## Modalità inferenza

### Modalità LLM (con OPENAI_API_KEY)
Il testo viene inviato a GPT (o al modello configurato) con un prompt strutturato che chiede di restituire codici ICD in formato JSON con punteggio di confidenza e razionale clinico.

### Modalità euristica (default, senza chiave API)
Usa la ricerca testuale locale sul dataset integrato. Calcola uno score per ogni codice in base alla corrispondenza delle parole chiave nella descrizione e normalizza i punteggi come confidenza relativa (0–1).

Il sistema è compatibile con qualsiasi endpoint OpenAI-compatible:
- **Ollama**: `LLM_BASE_URL=http://localhost:11434/v1`
- **LM Studio**: `LLM_BASE_URL=http://localhost:1234/v1`
- **Azure OpenAI**: configurare `LLM_BASE_URL` con l'endpoint Azure

