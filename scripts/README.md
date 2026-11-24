# scripts/

Strumenti di sviluppo (NON fanno parte del binario Go; non vengono embeddati).

## Benchmark di qualità (F7)

- `benchmark.json` — set di benchmark macchina-leggibile: query, modo e **criteri deterministici** attesi (entità nelle sub-query, off-topic, conteggi fatti/citazioni, provenienza, domini accademici).
- `eval.py` — valutatore deterministico (solo stdlib). Analizza una sessione o esegue un caso del benchmark via API e stampa una tabella **PASS/FAIL** + un **template di rubrica** da compilare a mano. Usa i metadati strutturati salvati dal server (`meta`: provenienza, mappa citazioni; dalla v2.5) e, per le sessioni precedenti, ricava le stesse informazioni dal testo.
- `judge.py` — **giudice di fedeltà** (V25-T1.3): per ogni frase citata chiede a un modello se l'evidenza citata la sostiene (`SUPPORTED` / `PARTIAL` / `UNSUPPORTED`) ed elenca le frasi senza citazioni.
- `judge_links.py` — ogni frase della Deep a ciclo con un link giudicata sulla sola pagina che cita; le pagine lette dal modello fanno da controllo (audit del 05/11, `docs/V25_LOG_VALUTAZIONI.md`). Dal 13/11 il giudice vede la pagina intera (fino a 600.000 caratteri, prima 40.000): un giudizio fatto prima non si confronta con uno fatto dopo, e il rapporto lo dice nella prima riga (`docs/PIANO_DOPO_AUDIT.md` 5.2).
- `free_report.py NOME [NOME…]` — il rapporto **gratuito** di un giro (nessun giudice, nessuna rete, sola lettura): pagina ufficiale tra i risultati e letta, frasi e paragrafi con un link, ricerche vuote per tipo, letture, pagine d'errore lette, come si è chiusa ogni risposta. Con più giri mette le colonne affiancate; salva `out/free_NOME.json`. Le righe che chiedono la cache dicono n/a se la cache del giro non c'è più (`docs/PIANO_DOPO_AUDIT.md` 2.2). Se il giro contiene conversazioni (suite `conv`: sei casi con `turns` al posto di `query`, mandati da `eval.py` un turno dopo l'altro nella stessa sessione) aggiunge una tabella per turno: secondi, ricerche, pagine lette e rilette, bozze rimandate, numeri segnati nella nota e quanti di quelli stavano in una pagina letta in un turno prima (stesso controllo della Deep a ciclo, `claimnumbers -loop`), note di Vedetta scritte dal modello, lingua (`docs/PIANO_DOPO_AUDIT.md` 5.3). Giro di conversazioni: `python3 scripts/bench.py --tag NOME --suite conv --mode deep --live --no-judge`.
- `go run ./scripts/rerankreplay [CACHE_DIR…]` — rifà gratis il riordino dei risultati sulle ricerche salvate nella cache (che le tiene com'erano prima del riordino) e conta cosa cambia, anche per le pagine ufficiali di `expected.json` (piano 2.3, audit D8).
- `mixed_questions.json` — 20 domande a lingua mista (domanda in una lingua, titoli o nomi in un'altra) con la lingua attesa della risposta.
- `bench_instance.sh`, `bench.py`, `bench_report.py`, `compare_runs.py` — benchmark in replay su un'istanza separata, riepilogo con verdetto e confronto caso per caso (V25-R2, vedi sotto).
- `test_eval.py`, `test_judge.py`, `test_bench_report.py` — test unitari (`make eval-test`).

## Tre livelli di test (V25-R2)

| Livello | Domanda | Comando | Rete | Durata |
|---|---|---|---|---|
| 1. Unit test | il codice fa quello che dice? | `make test-offline` (Go) e `make eval-test` (script) | no | secondi |
| 2. Benchmark in replay | la versione B è migliore della A? | `python3 scripts/bench.py --tag NOME` | solo LLM e giudice | ~55 min a giro |
| 3. Smoke test live | la ricerca vera funziona ancora? | `make smoke` | sì | ~10 min |

### Livello 2: benchmark in replay

```bash
python3 scripts/bench.py --tag NOME --baseline base_c1015   # costruisce il programma da misurare in ~/vedetta-bench
python3 scripts/bench_report.py NOME --baseline base_c1015   # di nuovo il riepilogo, quando serve
python3 scripts/compare_runs.py NOME_1 NOME_2 --base-url http://localhost:3031   # perché i giri differiscono
```

- Gira su un'**istanza separata** in `~/vedetta-bench/`, porta 3031, cache in `replay`. Il servizio di produzione (3030) non viene toccato né riavviato.
- Il programma si costruisce dall'albero di lavoro **dentro `~/vedetta-bench/`**, mai con `make build`: `bin/vedetta` è il programma della produzione. `commit.txt` dice il commit, con `-dirty` se i file Go avevano modifiche non salvate; `meta_NOME.json` registra anche gli interruttori `VEDETTA_*` e i campi di `config.local` che dicono cos'è il giro (mai le chiavi), e `bench_report.py` stampa in testa quelli che differiscono tra i giri confrontati. Per misurare un programma vecchio: `VEDETTA_BENCH_BINARY` e `VEDETTA_BENCH_COMMIT`.
- Ogni giro riparte da una **copia fresca** del corpus congelato più recente (`data/cache-corpus-*`, oppure `--corpus DIR`): in replay i miss vengono salvati e un secondo giro sulla stessa copia sarebbe favorito.
- Il binario viene copiato in `~/vedetta-bench/` al momento della preparazione: ricompilare durante un giro non cambia nulla.
- L'istanza viene fermata sempre, anche con errori o con Ctrl+C. A mano: `scripts/bench_instance.sh status|stop`.
- File di uscita: `scripts/out/runs_NOME_N.json`, `eval_NOME_N.log`, `judge_NOME_N_<sessione>.txt`, `server_NOME_N.log` (log del server: le righe `[read]` dicono quali pagine sono state lette).
- Durante un giro meglio non usare Vedetta dalla UI: le due istanze usano lo stesso LLM (cambia la durata, non i risultati).
- Attenzione: il replay **non vede** un cambio di lettore o di ricerca, perché pagine e risultati vengono dalla cache. Quelle modifiche si misurano in rete.

**Regola di decisione** (`docs/SPEC_R2_TRE_LIVELLI_DI_TEST.md` §2.3):
1. una modifica si misura con **2 giri in replay** sullo stesso corpus della baseline;
2. si confronta la **percentuale aggregata** (SUPPORTED totali / frasi giudicate totali) con la baseline in replay;
3. conta solo una differenza di **almeno 4 punti**, con **entrambi i giri** dalla stessa parte della baseline; altrimenti "nessun effetto misurabile";
4. i singoli casi non decidono (oscillano di ~13 punti), servono solo a capire perché;
5. un giro con errori del giudice o casi mancanti è "non valido" e si ripete.

`bench_report.py` applica la regola e stampa il verdetto: `migliora`, `peggiora`, `nessun effetto misurabile`, `giro non valido`.

### Livello 3: smoke test live

`make smoke` esegue la suite `smoke` (3 casi brevi) sul servizio di produzione con 2 minuti di pausa tra un caso e l'altro, per non provocare limiti di frequenza. Non usa il giudice. Nel report di ogni caso: check `completes`, riga `search:` con ricerche vuote e motori che non rispondono.
La produzione è in `record`: le ricerche già fatte nelle ultime 24 ore arrivano dalla cache, quindi lo smoke test mette alla prova la rete solo sulle ricerche nuove (la riga `cache:` dice quante).

## Segnali e check

Ogni report stampa due righe di segnali:

- `signals:` fatti, citazioni (solo nel corpo della risposta, esclusa la bibliografia), fonti, provenienza, presenza dei metadati;
- `honesty:` quota di fatti citati, paragrafi sostanziali senza citazioni, link fuori dalle fonti della sessione, fatti il cui claim contiene numeri assenti dal proprio snippet (con i primi esempi).

Il "corpo" è la risposta del writer senza la coda aggiunta dal server (note metodologiche, bibliografia, nota di provenienza): vedi `TAIL_MARKERS` in `eval.py`, da tenere allineato alle intestazioni del codice Go.

| Tipo di check | Parametri | Passa se |
|---|---|---|
| `completes` | — | la risposta non è vuota |
| `subqueries_contain` | `value`, `case_insensitive` | ogni sotto-query contiene il valore |
| `facts_exclude_keywords` | `values` | nessun claim contiene le parole |
| `min_facts` / `min_citations` | `value` | conteggio ≥ valore |
| `provenance_in` | `values` | provenienza tra quelle ammesse |
| `domains_include_any` | `values` | almeno una fonte su quei domini |
| `answer_contains_any` | `values` | il corpo della risposta contiene uno dei valori |
| `cited_fact_ratio_min` | `value` | fatti citati / fatti estratti ≥ valore |
| `uncited_paragraph_ratio_max` | `value` | paragrafi sostanziali (> 200 caratteri, esclusi titoli, tabelle, citazioni) senza `[n]` né link ≤ valore |
| `links_in_sources` | — | ogni URL nel corpo è tra le fonti della sessione |
| `claim_numbers_in_snippet` | `value` (default 0) | quota di fatti con numeri del claim assenti dallo snippet ≤ valore (con `meta`: solo i fatti citati) |
| `answer_declares_not_found` | — | provenienza `parametric`/`degraded` oppure la risposta dichiara di non aver trovato evidenza |

Limiti noti dei segnali euristici:
- `claim_numbers_in_snippet` non riconosce i numeri scritti in lettere ("six" contro "6") né le conversioni di unità: è un indicatore da leggere con gli esempi stampati, non una prova;
- un paragrafo con una sola citazione conta come citato anche se contiene frasi non supportate: per la fedeltà frase per frase serve il giudice (T1.3).

## Uso

```bash
# Valuta una sessione già eseguita, applicando i check di un caso del benchmark
make eval SESSION=<session-id> CASE=valencia-family-4d

# Esegue un caso (POST /api/search) e poi lo valuta
make eval CASE=speranza-philosophy-clinical

# Esegue tutti i casi (lento: ~2-3 minuti a caso) e accoda i report
python3 scripts/eval.py --all --log scripts/out/eval.log --runs-out scripts/out/runs.json

# Solo la suite ridotta (4 casi storici + unanswerable-fictional + conflicting-numbers-it)
python3 scripts/eval.py --all --suite reduced --runs-out scripts/out/runs_reduced.json
```

Opzioni: `--session`, `--case`, `--all`, `--suite` (con `--all`: solo i casi della suite, vedi `suites` in `benchmark.json`), `--runs-out` (con `--all`: salva la mappa caso → sessione, utile per lanciare poi il giudice), `--base-url` (default `http://localhost:3030`), `--benchmark`, `--log`. Con `--all` un caso che fallisce non interrompe gli altri.

Il benchmark ha 17 casi su più domini e lingue (IT, EN, DE, FR), compresi due casi senza risposta che devono dichiarare l'assenza di evidenza. `answer_contains_any` cerca solo nel corpo della risposta, non nella coda.

Richiede il server Vedetta in esecuzione. La descrizione dei casi e i riferimenti alle run baseline vivono in `docs/V25_BENCHMARK.md` e `docs/V25_LOG_VALUTAZIONI.md` (locali).


## Giudice di fedeltà (`judge.py`)

```bash
make judge SESSION=<session-id>                 # una sessione
make judge SESSION="<id1> <id2> <id3>"          # più sessioni
```

Per ogni frase della risposta che contiene citazioni, il giudice riceve **solo** la frase, la frase precedente (per capire a cosa si riferisce, es. "questo valore") e gli estratti citati come prova:
- `[n]` → lo snippet verbatim del fatto citato (dalla mappa `meta`, o dalla bibliografia per le sessioni vecchie);
- link a URL → l'anteprima di ricerca di quella fonte (Tier 1, non il testo completo).

Non riceve la domanda dell'utente né il nome del modello che ha scritto la risposta: deve dire se la frase sta in piedi **sull'evidenza che il lettore può controllare**.

Esiti per frase: `SUPPORTED`, `PARTIAL` (il punto principale c'è, ma la frase aggiunge dettagli, numeri o qualificatori assenti), `UNSUPPORTED`, `NO_EVIDENCE` (cita qualcosa che non esiste), `UNCITED` (frase di almeno 40 caratteri senza citazioni: non giudicata, ma elencata). La metrica principale è **% SUPPORTED** sulle frasi giudicate. I risultati completi vanno in `scripts/out/judge_<sessione>.json` (escluso da git).

### Configurazione del giudice

In ordine di precedenza:
1. variabili d'ambiente `JUDGE_BASE_URL`, `JUDGE_MODEL`, `JUDGE_API_KEY`, `JUDGE_REASONING_EFFORT` (default `low`);
2. file `scripts/judge.env` con le stesse righe `CHIAVE=valore` (escluso da git; proteggerlo con `chmod 600`);
3. in mancanza di entrambi, l'endpoint LLM di default di Vedetta (`/api/config`), con `reasoning_effort: none`. Funziona, ma il giudice è della stessa famiglia del writer e quindi meno indipendente.

Esempio di `scripts/judge.env` (provider OpenAI-compatibile qualsiasi):
```
JUDGE_BASE_URL=https://api.entrim.ai/v1
JUDGE_MODEL=zai-org/GLM-5.3-Flash
JUDGE_API_KEY=...
```

**Regola:** si confrontano solo misure fatte con lo **stesso giudice** (il modello è stampato nell'intestazione del report e salvato nel JSON). Un giudice nuovo va tarato: leggere a mano una decina di frasi `PARTIAL`/`UNSUPPORTED` e una decina di `SUPPORTED` e verificare che abbia ragione almeno 8 volte su 10.
