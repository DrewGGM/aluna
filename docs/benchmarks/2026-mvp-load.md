# Prueba de carga HTTP del MVP (SM-3, SM-4) — 2026-11-13

Mide la latencia de los postings y de las consultas de saldo a través de la API, con el binario compilado,
a una tasa constante de 300 postings/s durante 10 minutos.

## Umbral (fijado antes de correr)

| Métrica | Umbral |
|---|---|
| Latencia p99 de posting (SM-3) | < 200 ms |
| Latencia p99 de consulta de saldo (SM-4) | < 100 ms |
| Respuestas 5xx | 0 % |
| Tasa sostenida de postings | ≥ 99 % de 300/s |
| Postings asentados | Todos los que la API respondió con 201, una vez cada uno |

## Carga

- `aluna api` y `aluna worker` como procesos, compilados sin `-race` (como se despliegan), conectados con el
  rol `aluna_app` a una base nueva migrada con `aluna migrate up`. El worker corre sus jobs como en
  producción: agrega los saldos async, publica el outbox y sella la auditoría mientras entra la carga.
- Una entidad legal creada con SQL como dueño de las tablas, con fecha de negocio de hoy: la prueba corre
  cualquier día, también un fin de semana o un festivo, y `aluna entity create` solo acepta un día hábil. Los
  jobs del worker que recorren entidades (controles, GL, causación) corren sobre ella durante la carga.
- Tokens de un emisor OIDC de prueba (`oidctest`) con el rol `operator` de esa entidad.
- Las cuentas siguen QS-3: 1.000 clientes sync (pasivos con un sobregiro amplio: el límite se valida en cada
  posting, pero no hace falta fondearlos), una cuenta settlement async (activo) y una de ingresos async,
  creadas por la API. Con solo dos cuentas sync, todos los postings bloquearían los mismos dos saldos y la
  prueba mediría esa contención y no la de QS-3.
- Cada posting es un `POST /v1/transactions` de 4 entries con `Idempotency-Key` y referencia externa al
  azar: un cliente paga 1.000, otro recibe 900, 90 van al settlement y 10 a ingresos. Los dos clientes se
  eligen al azar.
- Además, 50 consultas por segundo de `GET /v1/ledger-accounts/{id}/balance` de un cliente al azar.
- Carga abierta: cada solicitud sale cuando le toca (la n-ésima, contando desde 0, a los n/300 s; el
  generador revisa cada milisegundo), responda o no la API la anterior. La latencia se cuenta desde ese
  instante, así que un cliente atrasado suma su atraso.
- Al terminar, la prueba cuenta en `ledger.transactions` los postings de la carga y exige que sean
  exactamente los que la API respondió con 201.

Código: `test/load/posting_load_test.go`. Exige `ALUNA_TEST_DATABASE_URL` con un PostgreSQL durable (se
niega a medir sin `fsync` ni `synchronous_commit`); `ALUNA_LOAD_DURATION` acorta la corrida en una
estación de trabajo (por defecto, 10 minutos). La corrida medida:

```sh
ALUNA_TEST_DATABASE_URL="postgres://aluna:aluna@localhost:55432/aluna?sslmode=disable" \
  go test -count=1 -tags=load -timeout=20m -v ./test/load/...
```

Sin `-v`, `go test` no muestra las cifras de una corrida que pasa.

## Entorno

| | |
|---|---|
| Equipo | Laptop AMD Ryzen 7 260 (8 núcleos, 16 hilos), 31 GB de RAM, Windows 11 Home |
| Base | PostgreSQL 16.15 (imagen `postgres:16`, configuración por defecto, `max_connections=100`) en Docker Desktop (VM de 16 CPU y 3,8 GiB), publicado en `localhost:55432` |
| Procesos | `aluna api` y `aluna worker` en Windows, Go 1.26.11, pool pgx por defecto (16 conexiones cada uno) |
| Cliente | El mismo `go test`, en el mismo equipo |

`fsync` y `synchronous_commit` en `on` (los registra la prueba). No es hardware de producción: la base, la
API, el worker y el generador de carga comparten el equipo.

## Resultados

| Métrica | Postings | Consultas de saldo | Umbral | |
|---|---|---|---|---|
| Enviadas | 180.000 en 10 min | 30.000 en 10 min | — | |
| Respondidas como se esperaba | 180.000 (201) | 30.000 (200) | todas | ✔ |
| 5xx | 0 | 0 | 0 % | ✔ |
| Otros estados o sin respuesta | 0 | 0 | — | |
| Tasa respondida | 300,0/s | 50,0/s | ≥ 297/s en postings | ✔ |
| Latencia p50 | 15,1 ms | 2,3 ms | — | |
| Latencia p95 | 17,9 ms | 3,2 ms | — | |
| Latencia p99 | 20,7 ms | 4,0 ms | < 200 ms / < 100 ms | ✔ |
| Latencia máxima | 196,2 ms | 194,7 ms | — | |
| Postings en el ledger | 180.000 | — | los 201 | ✔ |

## Lectura

- Cumple los cinco umbrales con margen: el p99 de posting es la décima parte del límite y el de saldo la
  vigésima quinta.
- El posting cuesta unos 15 ms casi fijos: es una transacción con `fsync` que bloquea dos saldos de cliente,
  inserta sus 4 entries, el evento del outbox, su job de River y el registro de auditoría. La consulta de
  saldo lee una fila y tarda 2–3 ms.
- Las distribuciones son estrechas (p50 y p99 a menos de 6 ms en postings): a 300/s no hay cola, cada
  posting encuentra conexión libre y casi nunca coincide con otro sobre el mismo cliente. Con carga cerrada
  y 64 goroutines, el benchmark de QS-3 (`2026-posting.md`) dio un p50 de 67,9 ms por la espera de
  conexión, que aquí no aparece.
- Los dos máximos, cerca de 195 ms, son casos aislados en un equipo compartido por la base, la API, el
  worker y el generador; no se investigó su causa.
- La tasa sostenida es casi una consecuencia del modelo abierto: el generador envía exactamente las
  180.000, así que la tasa respondida solo baja si fallan solicitudes (lo que ya exigen las otras filas) o
  si las últimas respuestas llegan segundos tarde.
- Una primera corrida de esta misma prueba dio 23,9 ms de p99 en postings y 23,8 ms en saldo: su generador
  enviaba cada solicitud un intervalo tarde (3,3 ms a 300/s y 20 ms a 50/s) y lo contaba como latencia. Se
  corrigió antes de esta medida.

## Pendiente

- Repetir en hardware de referencia de producción, con la base en otra máquina y el generador de carga
  fuera del equipo de la API. Dentro de la VM de Docker Desktop, `fsync=on` no garantiza que el dato llegue
  al disco del equipo.
- Medir con una cuenta sync caliente (Q-83), con rechazos (Q-87 a) y con varios endpoints de webhooks
  (Q-134 c).
- Medir con la tasa de diseño (2.000/s) y con un mes grande (los controles, el GL, el cierre de periodo y los
  extractos sobre un journal largo: Q-127 d, Q-130 f, Q-131 b y Q-132 e).
