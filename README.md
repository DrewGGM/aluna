# Aluna

Core bancario en Go para entidades financieras colombianas: ledger de doble partida inmutable,
depósitos, comisiones y GMF, pagos, reconciliación y auditoría, con operación continua 24/7.

En la tradición kogui y arhuaca de la Sierra Nevada de Santa Marta, *aluna* es el plano del
pensamiento y la memoria donde todo existe antes de existir. Un ledger es eso: la memoria completa e
inmutable de cada movimiento de dinero.

## Estado

El MVP (etapas M0 a M5) está completo y probado de punta a punta, pero todavía no sirve para operar con
dinero real. Antes hay que reemplazar cuatro piezas de desarrollo:

- **Rail de pagos:** los pagos salen y entran por un rail simulado (`simbreb`); falta el conector del rail
  real y la liquidación de su cuenta clearing.
- **Screening SARLAFT:** con `ALUNA_SCREENING_VENDOR=lists` filtra contra OFAC SDN, la lista consolidada de
  la ONU y los PEP de datos.gov.co, descargadas cada 6 horas (ADR-0018). Necesita el servicio de identidad de
  la entidad, que dice quién es cada sujeto. Sin esa variable usa un proveedor falso de desarrollo.
- **Ancla de auditoría:** los hashes de los bloques sellados van a un directorio (`ALUNA_AUDIT_ANCHOR_DIR`)
  en desarrollo, o en producción a un bucket S3 con Object Lock en modo compliance
  (`ALUNA_AUDIT_ANCHOR_S3_URL`), que nadie puede reescribir ni borrar durante su retención. Falta crear ese
  bucket en la cuenta de la entidad.
- **IAM:** el realm de Keycloak de [`deploy/keycloak/`](deploy/keycloak/realm-aluna.json) es de
  desarrollo, con usuarios y secretos fijos; producción necesita un emisor OIDC real con los claims
  `legal_entity_id` y `roles`.

Límites conocidos: el rol `channel` aún no ve recursos propios (todo le responde 404); las transferencias
recibidas por llave van a la cuenta de suspense porque no hay directorio de llaves; y los asientos manuales
(`POST /v1/transactions`, roles `operator`, `finance` y `system`) solo pasan por maker-checker desde el umbral
por moneda de `ALUNA_MANUAL_POSTING_APPROVAL`, que sin valor no aplica; el umbral se mide por asiento (varios
asientos menores no lo alcanzan), y un hold y su captura (`POST /v1/holds`) mueven dinero sin pasar por él.

| Etapa | Qué entregó |
|---|---|
| M0 | Esqueleto: CI, HTTP, PostgreSQL, migraciones, una transacción balanceada de punta a punta |
| M1 | Ledger completo: idempotencia, reversas, holds de dos fases, cuentas calientes, benchmark |
| M2 | Calendario y periodos, outbox y jobs, auditoría encadenada, IAM, RBAC, maker-checker, clientes |
| M3 | Productos versionados, cuentas de depósito, interés, comisiones y GMF |
| M4 | Screening SARLAFT, pagos salientes y entrantes, reconciliación |
| M5 | Contabilidad proyectada a CUIF, cierre de periodo, extractos, webhooks, observabilidad |

## Diseño

Aluna es un **monolito modular hexagonal**: un solo binario, una base PostgreSQL por entidad regulada y
módulos con límites estrictos.

- **Un módulo por contexto de negocio** en `internal/<módulo>/`. Cada uno expone una fachada; su
  dominio, casos de uso y adaptadores viven en `internal/<módulo>/internal/` y nadie más los importa.
- **El dominio no depende de nada externo**: solo de la biblioteca estándar y de los tipos propios de
  dinero e identificadores. Los adaptadores (PostgreSQL, HTTP, rails de pago) se conectan por puertos.
- **Un binario, varios procesos**: `aluna api`, `aluna worker`, `aluna migrate`, `aluna entity create` y
  `aluna audit verify` son subcomandos del mismo ejecutable.

| Garantía del ledger | Cómo se cumple |
|---|---|
| El dinero no se crea ni se destruye | Cada transacción suma cero por moneda; la base lo verifica con un trigger diferido |
| Lo asentado no cambia | Los entries son *append-only*; una corrección es una reversa enlazada al original |
| Ninguna operación se aplica dos veces | Todo comando que mueve dinero exige `Idempotency-Key` o `(source, external_ref)` |
| Sin errores de redondeo | Montos en `int64` de unidades menores; el cálculo intermedio usa decimal exacto, nunca `float` |
| Sin cierre nocturno | Fecha de negocio lógica por entidad y cambio de día por eventos, compatible con Bre-B 24/7 |

## Requisitos

- Go 1.26 o superior
- Docker, para PostgreSQL local y las pruebas de integración y E2E

## Uso

```bash
go build -o aluna ./cmd/aluna
docker compose -f deploy/compose.yaml up -d postgres   # ALUNA_PG_PORT=55432 si el 5432 está ocupado
docker compose -f deploy/compose.yaml --profile iam up -d keycloak   # IAM de desarrollo en :8180
ALUNA_DATABASE_URL="postgres://aluna:aluna@localhost:5432/aluna?sslmode=disable" ./aluna migrate up
# la entidad legal, con el dueño de las tablas y el id que llevan los tokens del realm de prueba; la fecha
# de negocio inicial es un día hábil que ya empezó en America/Bogota (hoy o antes): aquí, el 2026-11-13
ALUNA_DATABASE_URL="postgres://aluna:aluna@localhost:5432/aluna?sslmode=disable" \
  ./aluna entity create --id 01a00000-0000-7000-8000-000000000001 --name "Entidad de prueba" \
  --business-date 2026-11-13 --currency COP
# api y worker corren con el rol aluna_app, que no puede modificar ni borrar lo asentado; el secreto del
# rail simulado es el mismo para los dos
export ALUNA_SIMBREB_SECRET="$(openssl rand -hex 32)"
app_dsn="postgres://aluna:aluna@localhost:5432/aluna?sslmode=disable&options=-c%20role%3Daluna_app"
ALUNA_DATABASE_URL="$app_dsn" \
ALUNA_OIDC_ISSUER=http://localhost:8180/realms/aluna ALUNA_OIDC_AUDIENCE=aluna-api ./aluna api &
# el worker, con un directorio ya existente para el ancla de auditoría; si se borra la base, se borra también
# tmp/anchor: el worker no arranca con un ancla que tiene bloques que la base no tiene
mkdir -p tmp/anchor && ALUNA_DATABASE_URL="$app_dsn" ALUNA_AUDIT_ANCHOR_DIR=tmp/anchor ./aluna worker &
until curl -sf localhost:8080/readyz; do sleep 1; done   # {"status":"ok"} cuando la API llega a la base
# el token del realm de prueba (usuario operator/operator), para el header Authorization: Bearer
TOKEN=$(curl -s -d grant_type=password -d client_id=aluna-dev -d username=operator -d password=operator \
  http://localhost:8180/realms/aluna/protocol/openid-connect/token | sed -E 's/.*"access_token":"([^"]+)".*/\1/')
# el del programador de tareas (cliente aluna-scheduler/scheduler, rol system), para el rollover
SCHEDULER_TOKEN=$(curl -s -d grant_type=client_credentials -d client_id=aluna-scheduler \
  -d client_secret=scheduler http://localhost:8180/realms/aluna/protocol/openid-connect/token | sed -E 's/.*"access_token":"([^"]+)".*/\1/')
```

Antes del primer pago, crea las cuentas del rail simulado con el token de `operator`, como dice
[Alta de una entidad](docs/arquitectura.md#alta-de-una-entidad); sin ellas, `POST /v1/payment-orders`
responde 500. El rollover del día se hace con el token del programador de tareas y la fecha que cierra,
`{"business_date": "2026-11-13"}` ([El día de negocio](docs/arquitectura.md#el-día-de-negocio)).

La configuración completa está en [Configuración](docs/arquitectura.md#configuración), y la versión se inyecta
al compilar con `-ldflags "-X main.version=0.1.0"`. La imagen Docker se construye con
`docker build --build-arg VERSION=0.1.0 -t aluna:0.1.0 .`; cómo correrla está en
[Contenedores](docs/arquitectura.md#contenedores).

## Pruebas

```bash
go test -race -count=1 ./...                               # unitarias
go test -race -count=1 -tags=integration ./...             # con PostgreSQL real (Docker)
go test -race -count=1 -tags=e2e ./test/e2e/...            # el binario de punta a punta
# carga HTTP de 10 min a 300 postings/s, contra un PostgreSQL durable
ALUNA_TEST_DATABASE_URL="postgres://aluna:aluna@localhost:55432/aluna?sslmode=disable" \
  go test -count=1 -tags=load -timeout=20m -v ./test/load/...
go test -count=1 -tags=restore -timeout=30m ./test/restore/...   # ensayo de restauración PITR (Docker)
```

Sin `ALUNA_TEST_DATABASE_URL`, las pruebas de integración y E2E levantan PostgreSQL 16 con testcontainers.
La prueba de carga exige esa variable y `ALUNA_LOAD_DURATION` acorta su corrida; los resultados medidos
están en [`docs/benchmarks/`](docs/benchmarks/2026-mvp-load.md).

## Documentación

- [`api/openapi.yaml`](api/openapi.yaml): el contrato de la API, con el permiso que exige cada operación.
- [`docs/arquitectura.md`](docs/arquitectura.md): procesos y configuración, alta de una entidad, módulos y
  sus endpoints, flujo de un comando y maker-checker, eventos y webhooks, jobs del worker, día de negocio,
  seguridad y roles, métricas y despliegue.
- [`docs/runbook.md`](docs/runbook.md): alertas, diagnóstico y procedimientos ante incidentes, incluida la
  restauración PITR.
- [`docs/postman/`](docs/postman/README.md): colección con una petición por operación y entorno de
  desarrollo.
- [`docs/benchmarks/`](docs/benchmarks/): mediciones de posting y de carga HTTP.

## Licencia

[Apache License 2.0](LICENSE).
