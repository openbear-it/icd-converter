# ICD Converter

Servizio REST in Go per la conversione tra codici **ICD-9-CM** e **ICD-10-IM** e la ricerca di codici **CIPI** (Classificazione degli Interventi e Procedure Italiani), con ricerca testuale full-text e ricerca per **similarità semantica** (embedding vettoriale o fallback euristico).

I dati provengono dalle fonti ufficiali del **Ministero della Salute italiano**: 16.212 diagnosi ICD-9-CM, 4.460 procedure ICD-9-CM, 14.773 codici ICD-10-IM, 18.189 mappature di transcodifica e 259 codici CIPI (versione GAMMA 2.1, valida dal 16/02/2026), tutti incorporati nel binario via `go:embed`.

## Funzionalità

| Feature | Descrizione |
|---|---|
| Conversione ICD-9 → ICD-10 | Dato un codice ICD-9 restituisce i codici ICD-10 equivalenti |
| Conversione ICD-10 → ICD-9 | Dato un codice ICD-10 restituisce i codici ICD-9 equivalenti |
| Espansione gerarchica | Dato un codice padre restituisce tutti i sottocodici (ICD-9, ICD-10, CIPI) |
| Ricerca testuale | Cerca codici per parola chiave in ICD-9, ICD-10 e/o CIPI (ranking BM25) |
| Ricerca semantica | Similarità vettoriale (embedding) con **text enrichment**, **query expansion LLM** e **fusione ibrida RRF** (embedding + BM25); fallback euristico quando l'embedding non è configurato |
| CIPI lookup/esplorazione | Lookup puntuale, espansione gerarchica e lista paginata dei codici CIPI |
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
│       ├── icd10im_mappings.csv
        └── cipi_codes.csv       # Codici CIPI (Classificazione degli Interventi e Procedure Italiani)
├── internal/
│   ├── db/
│   │   ├── db.go                # Apertura SQLite e schema migration (include tabella cipi_codes)
│   │   ├── seed.go              # Seeder idempotente da CSV embedded (include import CIPI)
│   │   └── loader.go            # Carica icd.Store dal DB (include LoadCIPI)
│   ├── icd/
│   │   ├── data.go              # Strutture ICDEntry, CIPIEntry e SearchResult
│   │   └── store.go             # Lookup, conversione, ricerca BM25 (ICD-9, ICD-10, CIPI)
│   ├── embed/
│   │   └── embed.go             # Builder embedding, Index cosine-similarity, multi-query fusion, cache SQLite
│   └── api/
│       ├── handlers.go          # Handler REST (conversione, lista, ricerca, expand, CIPI)
│       └── semantic.go          # Handler POST /search/semantic (embedding + fallback, CIPI)
├── web/
│   ├── index.html               # UI web (go:embed)
│   ├── openapi.yaml             # Spec OpenAPI 3.0.3 (versione 1.1.0)
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

### Generali

| Variabile | Default | Descrizione |
|---|---|---|
| `PORT` | `8080` | Porta HTTP |
| `GIN_MODE` | `release` | `debug` per log verbose di Gin |
| `ICD_DB_PATH` | `icd.db` | Percorso del database SQLite |

### LLM / Modello di linguaggio (chat)

| Variabile | Default | Descrizione |
|---|---|---|
| `LLM_API_KEY` | *(vuoto)* | API key per l'endpoint OpenAI o compatibile (Ollama, LM Studio, ecc.). Con Ollama locale si può lasciare a `ollama` o qualsiasi stringa non vuota |
| `LLM_BASE_URL` | endpoint OpenAI | URL base dell'endpoint compatibile OpenAI, es. `http://localhost:11434/v1` per Ollama |
| `LLM_MODEL` | `gpt-4o-mini` | Modello chat usato per la **query expansion semantica**. Deve essere un modello in grado di generare testo (non un embedding model) |
| `LLM_TIMEOUT` | `30` | Timeout in secondi per le chiamate al modello chat |

### Embedding (ricerca semantica)

| Variabile | Default | Descrizione |
|---|---|---|
| `LLM_EMBED_MODEL` | *(vuoto)* | Modello embedding. Se assente si usa la modalità BM25 euristica |
| `LLM_EMBED_BASE_URL` | valore di `LLM_BASE_URL` | URL endpoint embedding (può differire dall'endpoint chat) |
| `LLM_EMBED_API_KEY` | valore di `LLM_API_KEY` | API key per l'endpoint embedding |
| `LLM_EMBED_ENRICH_TEXT` | `true` | Arricchisce il testo indicizzato con la categoria/capitolo ICD prima dell'embedding. Impostare `false` per disabilitare (usa un namespace di cache separato) |

Modelli embedding consigliati (Ollama, ARM64-friendly):
- `nomic-embed-text` — 768 dim, ~274 MB, migliore qualità
- `all-minilm` — 384 dim, ~45 MB, minimo RAM, avvio rapido

> **Nota**: `LLM_API_KEY` e `LLM_BASE_URL` configurano **sia** il modello chat (query expansion) **sia** l'embedding se non si impostano le varianti `LLM_EMBED_*` specifiche. Con Ollama tutto locale è sufficiente impostare `LLM_BASE_URL=http://localhost:11434/v1`, `LLM_API_KEY=ollama`, `LLM_MODEL=<modello-chat>`, `LLM_EMBED_MODEL=<modello-embed>`.

## API Reference

### Conversione e lookup ICD

```
GET /api/v1/icd9/{code}               → lookup codice ICD-9
GET /api/v1/icd9/{code}/to-icd10      → converte ICD-9 → ICD-10
GET /api/v1/icd9/{code}/expand        → codice padre + tutti i sottocodici
GET /api/v1/icd10/{code}              → lookup codice ICD-10
GET /api/v1/icd10/{code}/to-icd9      → converte ICD-10 → ICD-9
GET /api/v1/icd10/{code}/expand       → codice padre + tutti i sottocodici
```

### CIPI — Classificazione degli Interventi e Procedure Italiani

```
GET /api/v1/cipi/{code}               → lookup codice CIPI
GET /api/v1/cipi/{code}/expand        → codice padre + figli diretti
GET /api/v1/cipi?page=1&limit=20&type=procedura   → lista paginata (type: diagnosi | procedura)
```

### Lista / Paginazione

```
GET /api/v1/icd9?page=1&limit=20&category=Malattie+del+sistema+circolatorio
GET /api/v1/icd10?page=1&limit=20
```

### Ricerca testuale

```
GET /api/v1/search?q=infarto&version=all
# version: "icd9" | "icd10" | "cipi" | "both" (icd9+icd10, default) | "all" (icd9+icd10+cipi)
# cipi_type: "diagnosi" | "procedura"  (solo per risultati CIPI)
```

### Ricerca semantica

```
POST /api/v1/search/semantic
Content-Type: application/json

{
  "q":         "paziente con dolore toracico, elevazione ST, infarto anteriore",
  "version":   "all",
  "limit":     10,
  "mode":      "auto",
  "cipi_type": "procedura"
}
```

Il campo `version` può essere:
- `all` *(default)* — ICD-9-CM + ICD-10-IM + CIPI
- `both` — solo ICD-9-CM + ICD-10-IM (retrocompatibile)
- `icd9` / `icd10` / `cipi` — solo la classificazione specificata

Il campo `cipi_type` filtra i risultati CIPI:
- `""` *(default)* — sia diagnosi che procedure
- `diagnosi` — solo procedure diagnostiche CIPI
- `procedura` — solo interventi/procedure terapeutiche CIPI

Il campo `mode` può essere:
- `auto` *(default)* — usa embedding se disponibile, altrimenti BM25 euristica
- `embedding` — solo vettoriale; restituisce 503 se il modello non è configurato
- `heuristic` — sempre BM25 keyword scoring

> **Pipeline ricerca semantica avanzata**: quando sia `LLM_EMBED_MODEL` che un modello LLM chat sono configurati, la ricerca esegue tre passi aggiuntivi rispetto all'embedding diretto:
>
> 1. **Query expansion** — l'LLM genera 2–3 riformulazioni ICD-aligned della query originale in italiano (bridging del divario lessicale tra linguaggio clinico e terminologia ICD ufficiale).
> 2. **Multi-query splitting** — ogni variante (originale + espansioni) superiore a 80 caratteri viene spezzata in clausole semanticamente coerenti su `.` `;` `\n` e `,`. L'intero insieme di vettori viene passato all'indice.
> 3. **Fusione ibrida RRF** — i risultati dell'embedding vengono fusi con quelli BM25 tramite **Reciprocal Rank Fusion** ($k=60$): i codici che compaiono in entrambe le liste ottengono un punteggio più alto. Questo garantisce che corrispondenze esatte di termini tecnici non vengano penalizzate dall'embedding.
>
> Per query brevi senza LLM il comportamento è identico all'embedding singolo.

Risposta:
```json
{
  "query": "...",
  "icd9_results":  [{ "code": "410.1", "description": "...", "score": 0.91 }],
  "icd10_results": [{ "code": "I21.0", "description": "...", "score": 0.89 }],
  "cipi_results":  [{ "code": "H1.1",  "description": "Bypass aortocoronarico (CABG)", "category": "procedura", "score": 0.95 }],
  "model": "nomic-embed-text"
}
```

> **Nota sui codici CIPI in ricerca semantica**: nei risultati `cipi_results` il campo `category` riporta il tipo CIPI (`diagnosi` o `procedura`). Per i metadati completi (campo `parent`, ecc.) usa `GET /api/v1/cipi/{code}`.

## Esempi curl

```bash
# Lookup ICD / CIPI
curl http://localhost:8080/api/v1/icd9/410.1
curl http://localhost:8080/api/v1/icd10/I21.0
curl http://localhost:8080/api/v1/cipi/H1.1

# Conversione ICD
curl http://localhost:8080/api/v1/icd9/427.31/to-icd10
curl http://localhost:8080/api/v1/icd10/I21.9/to-icd9

# Espansione gerarchica
curl http://localhost:8080/api/v1/icd9/410/expand
curl http://localhost:8080/api/v1/icd10/I21/expand
curl http://localhost:8080/api/v1/cipi/H/expand

# Lista CIPI con filtro tipo
curl "http://localhost:8080/api/v1/cipi?type=procedura&limit=10"

# Ricerca testuale (tutte le classificazioni)
curl "http://localhost:8080/api/v1/search?q=fibrillazione+atriale&version=all"

# Ricerca testuale solo CIPI procedure
curl "http://localhost:8080/api/v1/search?q=endoscopia&version=cipi&cipi_type=procedura"

# Ricerca semantica su testo clinico lungo (multi-query automatico)
curl -X POST http://localhost:8080/api/v1/search/semantic \
  -H 'Content-Type: application/json' \
  -d '{"q":"Paziente di 72 anni con BPCO in riacutizzazione, dispnea a riposo, SpO2 86%. ECG: FA rapida. BNP elevato. Ricovero in semi-intensiva.","limit":10,"version":"all"}'

# Ricerca semantica solo procedure CIPI
curl -X POST http://localhost:8080/api/v1/search/semantic \
  -H 'Content-Type: application/json' \
  -d '{"q":"bypass coronarico rivascolarizzazione","version":"cipi","cipi_type":"procedura","limit":5}'
```

## Come funziona la ricerca

### Ricerca testuale — BM25

Il ranking si basa su **BM25**, lo standard de-facto per la ricerca full-text. Rispetto a un semplice conteggio di match, BM25 introduce due correzioni fondamentali:

**IDF** (Inverse Document Frequency): un token raro (es. "riacutizzazione") pesa molto di più di uno ubiquo (es. "del"). In formula:

$$\text{IDF}(t) = \log\left(\frac{N - df_t + 0.5}{df_t + 0.5} + 1\right)$$

dove $N$ è il totale dei codici e $df_t$ quanti ne contengono il token $t$.

**TF saturata**: la rilevanza cresce in modo sublineare con le occorrenze (la quinta ripetizione aggiunge molto meno della prima). Il parametro $b$ normalizza per la lunghezza della descrizione, così una descrizione lunga non avvantaggia artificialmente rispetto a una corta:

$$\text{score}(d,t) = \text{IDF}(t) \cdot \frac{tf \cdot (k_1 + 1)}{tf + k_1 \left(1 - b + b \cdot \frac{dl}{\text{avgdl}}\right)}$$

I parametri usati sono i valori standard: $k_1 = 1.5$, $b = 0.75$.

### Ricerca semantica — Pipeline avanzata

La ricerca semantica con embedding applica tre tecniche complementari:

#### 1. Text enrichment sull'indice

Ogni entry ICD nel database viene indicizzata come `"Descrizione. Categoria"` invece della sola descrizione foglia. Per esempio un codice ICD-10 come `I21.0` viene indicizzato come:

> *"Infarto miocardico acuto transmural della parete anteriore. Malattie ischemiche del cuore"*

Questo fornisce al modello di embedding il contesto del capitolo/blocco ICD, riducendo il divario tra linguaggio clinico e terminologia formale. I vettori arricchiti vengono salvati in un namespace di cache separato (`_enriched` suffix) per non invalidare cache precedenti.

#### 2. LLM query expansion

Quando un modello chat è configurato (`LLM_API_KEY` + `LLM_MODEL`), prima dell'embedding la query viene inviata all'LLM che produce 2–3 riformulazioni usando la terminologia ICD ufficiale italiana. Per esempio:

```
Query originale:  "infarto anteriore con sopraslivellamento ST"
Espansioni LLM:   "Infarto miocardico acuto transmural della parete anteriore"
                  "STEMI anteriore, occlusione arteria discendente anteriore"
```

Tutte le varianti (originale + espansioni) vengono embeddate e passate all'indice, coprendo più dello spazio vettoriale ICD.

#### 3. Multi-query splitting

Un testo clinico lungo come:

> *"Paziente con BPCO in riacutizzazione, dispnea a riposo, SpO2 86%, FA rapida, BNP elevato"*

viene spezzato in clausole semanticamente coerenti (separatori: `.` `;` `\n`, e `,` per frasi > 120 caratteri):

```
["Paziente con BPCO in riacutizzazione",
 "dispnea a riposo",
 "SpO2 86%",
 "FA rapida",
 "BNP elevato"]
```

Ciascuna clausola è embeddata separatamente; i punteggi coseno vengono sommati e normalizzati. Un codice rilevante per anche solo una clausola emerge nel ranking invece di essere sepolto dalla media.

#### 4. Fusione ibrida RRF (Reciprocal Rank Fusion)

I risultati dell'embedding vengono fusi con quelli BM25 usando la formula RRF standard (Cormack et al. 2009):

$$\text{score}_{\text{RRF}}(d) = \sum_{l \in \{\text{embed},\, \text{bm25}\}} \frac{1}{k + \text{rank}_l(d)}$$

con $k = 60$. I codici che compaiono in entrambe le liste ricevono un punteggio più alto. Questo garantisce che corrispondenze esatte di termini tecnici non vengano penalizzate dall'embedding, e che concetti semantici non trovati dal keyword search emergano lo stesso.

## Dati CIPI

Il file `data/official/cipi_codes.csv` contiene un campione rappresentativo dei codici CIPI organizzati nei capitoli ufficiali (A–R). Per sostituirlo con i dati ufficiali completi pubblicati dal Ministero della Salute, scaricare il file dalla pagina:

> https://www.salute.gov.it/nuovo/it/news-e-media/notizie/nuove-classificazioni-sanitarie-icd-10-im-e-cipi-pubblicate-le-versioni/

e sostituire `data/official/cipi_codes.csv` con il CSV scaricato nel formato colonne: `code,description,type,parent,is_billable`. Successivamente cancellare il database (`icd.db`) per forzare il re-seeding al riavvio.
