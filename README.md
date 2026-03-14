# ICD Converter

Servizio REST in Go per la conversione tra codici **ICD-9-CM** e **ICD-10-IM**, con ricerca testuale full-text e ricerca per **similarità semantica** (embedding vettoriale o fallback euristico).

I dati provengono dalle fonti ufficiali del **Ministero della Salute italiano**: 16.212 diagnosi ICD-9-CM, 4.460 procedure ICD-9-CM, 14.773 codici ICD-10-IM e 18.189 mappature di transcodifica (versione GAMMA 2.1, valida dal 16/02/2026), tutti incorporati nel binario via `go:embed`.

## Funzionalità

| Feature | Descrizione |
|---|---|
| Conversione ICD-9 → ICD-10 | Dato un codice ICD-9 restituisce i codici ICD-10 equivalenti |
| Conversione ICD-10 → ICD-9 | Dato un codice ICD-10 restituisce i codici ICD-9 equivalenti |
| Espansione gerarchica | Dato un codice padre restituisce tutti i sottocodici |
| Ricerca testuale | Cerca codici per parola chiave in entrambe le versioni |
| Ricerca semantica | Similarità vettoriale (embedding) o keyword euristica |
| Lista / Paginazione | Esplora codici per versione e categoria con paginazione |
| UI Web | Interfaccia HTML con tab: Converti, Ricerca, Similarità, Esplora, Info |
| API Docs | Swagger UI interattiva su `/ui/swagger.html` |

## Struttura del progetto

```
icd-converter/
├── main.go                      # Entry point, router, config da env
├── data/
│   └── official/                # CSV ufficiali incorporati nel binario (go:embed)
│       ├── diagnosi_icd9cm.csv
│       ├── procedure_icd9cm.csv
│       ├── icd10im_codes.csv
│       └── icd10im_mappings.csv
├── internal/
│   ├── db/
│   │   ├── db.go                # Apertura SQLite e schema migration
│   │   ├── seed.go              # Seeder idempotente da CSV embedded
│   │   └── loader.go            # Carica icd.Store dal DB
│   ├── icd/
│   │   ├── data.go              # Struttura ICDEntry e SearchResult
│   │   └── store.go             # Lookup, conversione, ricerca testuale
│   ├── embed/
│   │   └── embed.go             # Builder embedding, Index cosine-similarity, cache SQLite
│   └── api/
│       ├── handlers.go          # Handler REST (conversione, lista, ricerca, expand)
│       └── semantic.go          # Handler POST /search/semantic (embedding + fallback)
├── web/
│   ├── index.html               # UI web (go:embed)
│   ├── openapi.yaml             # Spec OpenAPI 3.0.3
│   └── swagger.html             # Swagger UI (CDN)
└── kubernetes/
    └── icd-converter.yaml       # Namespace, PVC, ConfigMap, Deployment, Service, Ingress
```

## Avvio rapido

```bash
# Build
go build -o icd-converter .

# Avvio (modalità euristica, senza embedding esterno)
./icd-converter

# Avvio con file .env
env $(grep -v '^#' .env | xargs) ./icd-converter
```

Il server parte su `http://localhost:8080`.

- UI: `http://localhost:8080/ui/`
- API Docs (Swagger): `http://localhost:8080/ui/swagger.html`
- Health: `http://localhost:8080/health`


## Variabili di ambiente

| Variabile | Default | Descrizione |
|---|---|---|
| `PORT` | `8080` | Porta HTTP |
| `GIN_MODE` | `release` | `debug` per log verbose di Gin |
| `ICD_DB_PATH` | `icd.db` | Percorso del database SQLite |
| `LLM_EMBED_MODEL` | *(vuoto)* | Modello embedding. Se assente si usa la modalità euristica |
| `LLM_EMBED_BASE_URL` | valore di `LLM_BASE_URL` | URL endpoint embedding (es. Ollama) |
| `LLM_EMBED_API_KEY` | valore di `OPENAI_API_KEY` | API key per l'endpoint embedding |

Modelli embedding consigliati (Ollama, ARM64-friendly):
- `nomic-embed-text` — 768 dim, ~274 MB, migliore qualità
- `all-minilm` — 384 dim, ~45 MB, minimo RAM, avvio rapido

## API Reference

La documentazione interattiva completa è disponibile su `/ui/swagger.html` (Swagger UI).

### Conversione e lookup

```
GET /api/v1/icd9/{code}               → lookup codice ICD-9
GET /api/v1/icd9/{code}/to-icd10      → converte ICD-9 → ICD-10
GET /api/v1/icd9/{code}/expand        → codice padre + tutti i sottocodici
GET /api/v1/icd10/{code}              → lookup codice ICD-10
GET /api/v1/icd10/{code}/to-icd9      → converte ICD-10 → ICD-9
GET /api/v1/icd10/{code}/expand       → codice padre + tutti i sottocodici
```

### Lista / Paginazione

```
GET /api/v1/icd9?page=1&limit=20&category=Malattie+del+sistema+circolatorio
GET /api/v1/icd10?page=1&limit=20
```

### Ricerca testuale

```
GET /api/v1/search?q=infarto&version=both
# version: "icd9" | "icd10" | "both" (default)
```

### Ricerca semantica

```
POST /api/v1/search/semantic
Content-Type: application/json

{
  "q":       "paziente con dolore toracico, elevazione ST, infarto anteriore",
  "version": "both",
  "limit":   10,
  "mode":    "auto"
}
```

Il campo `mode` può essere:
- `auto` *(default)* — usa embedding se disponibile, altrimenti euristica
- `embedding` — solo vettoriale; restituisce 503 se il modello non è configurato
- `heuristic` — sempre keyword scoring

Risposta:
```json
{
  "query": "...",
  "icd9_results":  [{ "code": "410.1", "description": "...", "score": 0.91 }],
  "icd10_results": [{ "code": "I21.0", "description": "...", "score": 0.89 }],
  "model": "nomic-embed-text"
}
```

## Esempi curl

```bash
# Lookup
curl http://localhost:8080/api/v1/icd9/410.1
curl http://localhost:8080/api/v1/icd10/I21.0

# Conversione
curl http://localhost:8080/api/v1/icd9/427.31/to-icd10
curl http://localhost:8080/api/v1/icd10/I21.9/to-icd9

# Espansione gerarchica
curl http://localhost:8080/api/v1/icd9/410/expand
curl http://localhost:8080/api/v1/icd10/I21/expand

# Ricerca testuale
curl "http://localhost:8080/api/v1/search?q=fibrillazione+atriale"

# Ricerca semantica
curl -X POST http://localhost:8080/api/v1/search/semantic \
  -H 'Content-Type: application/json' \
  -d '{"q":"paziente con polmonite batterica e insufficienza respiratoria","limit":5}'
```
