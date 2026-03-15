# Scripts di generazione CSV

Questa directory contiene gli script Python per scaricare i file ufficiali del Ministero della Salute
e convertirli nei CSV usati dall'applicazione.

## Prerequisiti

```bash
pip install openpyxl requests
```

## Script disponibili

| Script | Sorgente ufficiale | Output |
|--------|--------------------|--------|
| `generate_cipi_csv.py` | `6.TAB_CIPI_V.GAMMA_2.0.xlsx` | `data/official/cipi_codes.csv` |
| `generate_icd10im_csv.py` | `3.ELENCO_SISTEMATICO_ICD-10-IM_V.GAMMA_2.1_VALIDITA_DA_16022026.xlsx` | `data/official/icd10im_codes.csv` |
| `generate_icd10im_mappings_csv.py` | `4.TAB_TRANSCOD_ICD-9-CM-ICD-10-IM_V.GAMMA_2.1.XLSX` | `data/official/icd10im_mappings.csv` |

## Aggiornamento dati

Per aggiornare tutti i CSV con le versioni più recenti:

```bash
cd /path/to/icd-converter
python scripts/generate_cipi_csv.py
python scripts/generate_icd10im_csv.py
python scripts/generate_icd10im_mappings_csv.py
```

Dopo l'aggiornamento dei CSV, eliminare il database per forzare il re-seeding:

```bash
rm -f icd.db
go run main.go   # oppure avviare il binario
```

## Fonti ufficiali

Tutti i file sono scaricati dalla pagina:
https://www.salute.gov.it/new/it/tema/assistenza-ospedaliera-sdo/documentazione-tecnica/

### ICD-9-CM

I file ICD-9-CM (`diagnosi_icd9cm.csv` e `procedure_icd9cm.csv`) sono stati ricavati manualmente
dai file ufficiali pubblicati dalla pagina del Ministero della Salute:
https://www.salute.gov.it/new/it/tema/assistenza-ospedaliera/il-manuale-icd9cm/

La versione utilizzata è la **2007 con successive correzioni**, che rimane lo standard
per la codifica della SDO nel 2026. Non è disponibile uno script automatizzato perché
i file originali non hanno un formato XLSX strutturato uniforme tra le versioni.
