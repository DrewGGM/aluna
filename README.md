# Aluna

Core bancario en Go para entidades financieras colombianas: ledger de doble partida inmutable,
depósitos, comisiones y GMF, pagos, reconciliación y auditoría, con operación continua 24/7.

> **Estado:** en construcción. M0 está completo: una transacción balanceada viaja por HTTP hasta
> PostgreSQL y de vuelta, con logs, trazas y métricas. Los módulos llegan por etapas (ver
> [Hoja de ruta](#hoja-de-ruta)).

En la tradición kogui y arhuaca de la Sierra Nevada de Santa Marta, *aluna* es el plano del
pensamiento y la memoria donde todo existe antes de existir. Un ledger es eso: la memoria completa e
inmutable de cada movimiento de dinero.

## Diseño

Así está diseñado Aluna; cada pieza llega en la etapa de la [hoja de ruta](#hoja-de-ruta) que se
indica. Es un **monolito modular hexagonal**: un solo binario, una base PostgreSQL por entidad regulada
y módulos con límites estrictos.

- **Un módulo por contexto de negocio** en `internal/<módulo>/`. Cada uno expone una fachada; su
  dominio, casos de uso y adaptadores viven en `internal/<módulo>/internal/` y nadie más los importa.
- **El dominio no depende de nada externo**: solo de la biblioteca estándar y de los tipos propios de
  dinero e identificadores. Los adaptadores (PostgreSQL, HTTP, rails de pago) se conectan por puertos.
- **Un binario, varios procesos**: `aluna api`, `aluna migrate` y, desde M2, `aluna worker` son
  subcomandos del mismo ejecutable.

### Garantías del ledger (M1)

| Garantía | Cómo se cumplirá |
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
# la API corre con el rol aluna_app, que no puede modificar ni borrar lo asentado
ALUNA_DATABASE_URL="postgres://aluna:aluna@localhost:5432/aluna?sslmode=disable&options=-c%20role%3Daluna_app" \
ALUNA_OIDC_ISSUER=http://localhost:8180/realms/aluna ALUNA_OIDC_AUDIENCE=aluna-api ./aluna api
# un token del realm de prueba (usuario operador/operador), para el header Authorization: Bearer
curl -s -d grant_type=password -d client_id=aluna-dev -d username=operador -d password=operador \
  http://localhost:8180/realms/aluna/protocol/openid-connect/token
```

Cada request, salvo `/healthz` y `/readyz`, lleva un token OIDC del emisor configurado, con `iat` y una
vida de 15 min como mucho; la entidad legal con la que opera sale del claim `legal_entity_id` del token,
nunca de un header.

Cada operación del contrato declara el permiso que exige (`x-aluna-permission`), y
[`config/roles.yaml`](config/roles.yaml) dice qué roles lo dan: `channel`, `operator`, `finance`,
`compliance`, `auditor` y `system`, según el claim `roles` del token. Lo que ningún rol del token da se
niega con 403. Un canal (`channel`) solo ve los recursos de su `party_id`: el de otra party le responde
404, como si no existiera. Por ahora ningún recurso tiene dueño (las cuentas del ledger no son de una
persona), así que a un canal todo le responde 404. La política viaja dentro del binario: un cambio de
permisos es una versión nueva.

Los comandos sensibles pasan por maker-checker: quedan como solicitud pendiente
(`GET /v1/approvals`) y corren cuando otra persona, con el permiso de ese tipo de solicitud, los aprueba
(`POST /v1/approvals/{id}/approval`) o los descarta (`POST /v1/approvals/{id}/rejection`). Quien pidió
no puede decidir su propia solicitud, y la base lo impide aunque el código fallara; una solicitud vence a
los 7 días.

| Variable | Uso |
|---|---|
| `ALUNA_DATABASE_URL` | Base de la entidad (obligatoria para `api`, `worker`, `migrate` y `audit verify`): `migrate` con el dueño de las tablas, el resto con el rol `aluna_app` |
| `ALUNA_OIDC_ISSUER` | Emisor de los tokens (obligatoria para `api`); se descubre al arrancar |
| `ALUNA_OIDC_AUDIENCE` | Audiencia que deben traer los tokens (obligatoria para `api`) |
| `ALUNA_AUDIT_ANCHOR_DIR` | Directorio, ya existente, del ancla de los bloques de auditoría (obligatoria para `worker` y `audit verify`) |
| `ALUNA_HTTP_ADDR` | Dirección de la API; por defecto `:8080` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Si existe, trazas y métricas salen por OTLP/HTTP (y el resto de variables `OTEL_*`) |

El contrato de la API está en [`api/openapi.yaml`](api/openapi.yaml); los errores son
`application/problem+json` (RFC 9457) con un `code` estable y, si el request tiene traza, su `trace_id`.

La versión se inyecta al compilar:

```bash
go build -ldflags "-X main.version=0.1.0" -o aluna ./cmd/aluna
```

## Pruebas

```bash
go test -race -count=1 ./...                               # unitarias
go test -race -count=1 -tags=integration ./...             # con PostgreSQL real (Docker)
go test -race -count=1 -tags=e2e ./test/e2e/...            # el binario de punta a punta
```

Sin `ALUNA_TEST_DATABASE_URL`, las pruebas levantan PostgreSQL 16 con testcontainers.

## Hoja de ruta

| Etapa | Contenido |
|---|---|
| M0 | Esqueleto: CI, HTTP, PostgreSQL, migraciones, una transacción balanceada de punta a punta |
| M1 | Ledger completo: idempotencia, reversas, holds de dos fases, cuentas calientes, benchmark |
| M2 | Calendario y periodos, outbox y jobs, auditoría encadenada, IAM, RBAC, maker-checker, clientes |
| M3 | Productos versionados, cuentas de depósito, interés, comisiones y GMF |
| M4 | Screening SARLAFT, pagos salientes y entrantes, reconciliación |
| M5 | Contabilidad proyectada a CUIF, cierre de periodo, extractos, webhooks, observabilidad |

## Licencia

[Apache License 2.0](LICENSE).
