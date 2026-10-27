# Benchmark de posting (QS-3) — 2026-10-27

Mide cuántos postings por segundo asienta el ledger con una cuenta de settlement compartida, y con qué
latencia, antes de construir los productos encima.

## Umbral (fijado antes de correr)

| Métrica | Umbral |
|---|---|
| Throughput | ≥ 300 postings/s |
| Latencia p99 | < 200 ms |
| Reintentos (40001/40P01) | < 0,1 % |

Si no se cumple, se mitiga (group commit o sharding de la cuenta settlement) antes de seguir con los
productos.

## Carga

- 10.000 cuentas de cliente sync (pasivos), una cuenta settlement async (activo) y una de ingresos
  async. Los clientes tienen un sobregiro amplio: el límite se valida en cada posting, pero no hace
  falta fondearlos.
- Cada posting tiene 4 entries: un cliente paga 1.000, otro recibe 900, 90 van al settlement y 10 a
  ingresos por comisión. Los dos clientes se eligen al azar.
- Cada posting sigue el camino de `POST /v1/transactions` con `Idempotency-Key`, en una sola
  transacción: reclamo de la clave, savepoint, `Post` (bloqueo ordenado de los dos saldos sync,
  validación del límite, inserción) y la respuesta guardada. La clave y la referencia externa son al
  azar, como las de un cliente. No pasa por HTTP; la carga HTTP es aparte.
- 64 goroutines asentando a la vez durante al menos 10 minutos (`-benchtime=600s`; `go test` calibra
  `b.N` y la corrida medida duró ~870 s), con el agregador de saldos async corriendo cada segundo en
  paralelo, como lo hará el job. Es una carga de lazo cerrado: cada goroutine espera su posting antes
  del siguiente.
- Se cuenta cada intento de la transacción: los que pasan de uno son reintentos de `UnitOfWork`. Al
  terminar, el benchmark agrega lo que le faltaba al agregador y exige que el settlement tenga la suma
  de sus entries.

Código: `internal/ledger/bench_integration_test.go`. Corre contra un PostgreSQL durable (el de
testcontainers arranca con `fsync=off`, y el benchmark se niega a medir sin `fsync` ni
`synchronous_commit`), por ejemplo el de `deploy/compose.yaml`:

```sh
ALUNA_TEST_DATABASE_URL="postgres://aluna:aluna@localhost:${ALUNA_PG_PORT:-5432}/aluna?sslmode=disable" \
  go test -run=XXX -bench=BenchmarkPost -benchtime=600s -timeout=60m -tags=integration ./internal/ledger/...
```

`-timeout` hace falta: el límite por defecto de `go test` (10 min) corta una corrida de 10 minutos más
la calibración de `b.N`.

## Entorno

| | |
|---|---|
| Equipo | Laptop AMD Ryzen 7 260 (8 núcleos, 16 hilos), 31 GB de RAM, Windows 11 Home |
| Base | PostgreSQL 16.15 (imagen `postgres:16`, configuración por defecto) en Docker Desktop (VM de 16 CPU y 3,8 GiB) |
| Cliente | Go 1.26.10, pgx v5.11.0; pool de 16 conexiones (el valor por defecto de pgxpool: una por hilo) |
| Red | Cliente y base en el mismo equipo |

`fsync` y `synchronous_commit` en `on` (los registra el benchmark): cada commit espera el disco. No es
hardware de producción: la base comparte el equipo con el cliente y con otras aplicaciones.

## Resultados

| Métrica | Resultado | Umbral | |
|---|---|---|---|
| Postings asentados | 794.545 en ~870 s | — | |
| Throughput | 914,5 postings/s | ≥ 300 | ✔ |
| Latencia p50 | 67,9 ms | — | |
| Latencia p95 | 85,3 ms | — | |
| Latencia p99 | 102,4 ms | < 200 ms | ✔ |
| Reintentos | 0 % | < 0,1 % | ✔ |
| Atraso del agregador al parar | 1.484 entries (~0,8 s de postings) | — | |

## Lectura

- Cumple el umbral con margen: tres veces el throughput pedido y un p99 cerca de la mitad del límite.
- La latencia la domina la espera de una conexión: hay 64 goroutines y 16 conexiones. Por la ley de
  Little, 64 en curso a 914,5 por segundo dan una latencia media de unos 70 ms, cerca del p50 medido;
  con las 16 conexiones siempre ocupadas, cada transacción tiene la suya unos 17 ms. Como la carga es
  de lazo cerrado, latencia y throughput no son independientes: no se midió la latencia con una tasa
  de llegada fija de 300/s, que sería menor.
- No hubo reintentos, y es por diseño (ADR-0004): en READ COMMITTED no hay conflictos de serialización
  (40001), y como los saldos se bloquean en orden de cuenta tampoco hay deadlocks (40P01). Cuando dos
  postings tocan el mismo cliente, el segundo espera, no se reintenta; las cuentas compartidas
  (settlement e ingresos) son async y no se bloquean. Por eso una carga concentrada en pocas cuentas
  subiría la latencia, no los reintentos. Los reintentos del agregador no se miden; no compite con los
  postings: solo bloquea su cursor y los saldos async, que ningún posting bloquea.
- El agregador de un segundo se mantuvo al día: al parar le faltaba menos de un segundo de postings.
- Sin mitigación: no hace falta group commit ni sharding del settlement por ahora.
- Como referencia, una corrida anterior con `fsync=off` y claves ordenadas en el tiempo dio unos 1.140
  postings/s: esperar el disco (junto con el cambio de claves) cuesta aquí en torno al 15–20 % del
  throughput.

## Pendiente

- Repetir en hardware de referencia de producción, y con la carga HTTP completa.
- Medir el costo del agregador cuando el settlement acumula millones de entries, y el saldo bitemporal
  de una cuenta con historia larga.
