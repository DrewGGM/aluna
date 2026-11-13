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
# la entidad legal, también con el dueño de las tablas, con el id que llevan los tokens del realm de prueba;
# su fecha de negocio inicial es la de hoy si hoy es día hábil o, si no, la del último día hábil (aquí, un
# viernes 13 de noviembre de 2026)
ALUNA_DATABASE_URL="postgres://aluna:aluna@localhost:5432/aluna?sslmode=disable" \
  ./aluna entity create --id 01a00000-0000-7000-8000-000000000001 --name "Entidad de prueba" \
  --business-date 2026-11-13 --currency COP
# la API corre con el rol aluna_app, que no puede modificar ni borrar lo asentado; el secreto del rail
# simulado es el mismo para api y worker
export ALUNA_SIMBREB_SECRET="$(openssl rand -hex 32)"
ALUNA_DATABASE_URL="postgres://aluna:aluna@localhost:5432/aluna?sslmode=disable&options=-c%20role%3Daluna_app" \
ALUNA_OIDC_ISSUER=http://localhost:8180/realms/aluna ALUNA_OIDC_AUDIENCE=aluna-api ./aluna api
# un token del realm de prueba (usuario operator/operator), para el header Authorization: Bearer
curl -s -d grant_type=password -d client_id=aluna-dev -d username=operator -d password=operator \
  http://localhost:8180/realms/aluna/protocol/openid-connect/token
# el del programador de tareas (cliente aluna-scheduler/scheduler, rol system), para el rollover
curl -s -d grant_type=client_credentials -d client_id=aluna-scheduler -d client_secret=scheduler \
  http://localhost:8180/realms/aluna/protocol/openid-connect/token
```

Cada request, salvo `/healthz` y `/readyz`, lleva un token OIDC del emisor configurado, con `iat` y una
vida de 15 min como mucho; la entidad legal con la que opera sale del claim `legal_entity_id` del token,
nunca de un header.

Las entidades legales no se crean por la API (el rol `aluna_app` no puede insertarlas): las registra quien
opera el despliegue con `aluna entity create`, con el dueño de las tablas en `ALUNA_DATABASE_URL`, como
`migrate`. Recibe el nombre, la fecha de negocio inicial, que debe ser un día hábil que ya empezó en
America/Bogota (hoy o antes), y la moneda funcional (ISO 4217, que no cambia después), e imprime el id de
la entidad: el `legal_entity_id` que deben llevar los tokens de sus usuarios. Sin `--id` el id es nuevo; con
`--id`, la entidad toma el que ya emite el IAM, como el `01a00000-0000-7000-8000-000000000001` del usuario
`operator` y del cliente `aluna-scheduler` del realm de desarrollo.

Cada entidad tiene su fecha de negocio, la de sus asientos (ADR-0006). `GET /v1/business-date` la
consulta. Al final de cada día hábil, el programador de tareas del operador, con un cliente del IAM con rol
`system` (en desarrollo, `aluna-scheduler`), llama `POST /v1/business-date/rollover` con la fecha que
cierra (`{"business_date": "2026-10-30"}`) y su `Idempotency-Key`: la fecha pasa al siguiente día hábil,
sale el evento `aluna.calendar.business-date.rolled.v1` y la respuesta trae la fecha cerrada y la nueva. Un
día se cierra cuando ya empezó en America/Bogota: cerrar uno posterior a hoy responde 422
`business_date_ahead`, así que la fecha de negocio nunca se adelanta al calendario. Si la fecha no es la que
dice el cuerpo (otro rollover la avanzó, o quien llama va atrasado), responde 409 `business_date_stale`; en
los dos casos no cambia nada. Reintentar con la misma clave devuelve la misma respuesta sin avanzar otra
vez; un 409 o un 422, como todo 4xx de negocio, también queda guardado con su `Idempotency-Key`, así que el
programador de tareas usa una clave nueva para el intento siguiente (por ejemplo, cuando el día ya empezó).
Un token cuya entidad no está registrada recibe 404 `legal_entity_not_found`. Los procesos de fin de día del
worker corren sobre los días cerrados.

Las notificaciones de los rails no van por ese puerto sino por `ALUNA_RAIL_ADDR`, que no se expone a los
canales: el resultado de un pago saliente (`POST /v1/rails/simbreb/notifications`) y la transferencia que
otro banco manda a un cliente (`POST /v1/rails/simbreb/transfers`). El rail no trae token, firma cada
notificación (Standard Webhooks, HMAC-SHA256 con `ALUNA_SIMBREB_SECRET`), y una con otra firma, o firmada
hace más de 5 min, responde 401. El worker envía los pagos por el rail simulado, que notifica a
`ALUNA_SIMBREB_NOTIFY_URL`.

Una transferencia recibida se acredita a la cuenta de depósito que nombra su número (el id de la cuenta) si
está activa y en su moneda; si no (o si llega por llave, porque el directorio de llaves aún no existe), va a
la cuenta de suspense del rail (`L.SUSPENSE.SIMBREB`, que la entidad crea junto a `L.CLEARING.SIMBREB`) y
su orden queda `suspended`, con la razón. Las dos cuentas del rail son pasivos `async`: una transferencia
recibida debita la clearing, y una cuenta `sync` sin saldo la rechazaría.

Cada operación del contrato declara el permiso que exige (`x-aluna-permission`), y
[`config/roles.yaml`](config/roles.yaml) dice qué roles lo dan: `channel`, `operator`, `finance`,
`compliance`, `auditor`, `system` e `integrator`, según el claim `roles` del token. `system` es el de los
servicios internos y el programador de tareas; `integrator`, el de quien sigue los eventos de la entidad
(`GET /v1/events` y `POST /v1/webhook-endpoints`) y nada más, así que su token no mueve dinero. Lo que
ningún rol del token da se niega con 403. Un canal (`channel`) solo ve los recursos de su `party_id`: el de
otra party le responde 404, como si no existiera. Por ahora ningún recurso tiene dueño (las cuentas del
ledger no son de una persona), así que a un canal todo le responde 404. La política viaja dentro del
binario: un cambio de permisos es una versión nueva.

Los comandos sensibles pasan por maker-checker: quedan como solicitud pendiente
(`GET /v1/approvals`) y corren cuando otra persona, con el permiso de ese tipo de solicitud, los aprueba
(`POST /v1/approvals/{id}/approval`) o los descarta (`POST /v1/approvals/{id}/rejection`). Quien pidió
no puede decidir su propia solicitud, y la base lo impide aunque el código fallara; una solicitud vence a
los 7 días.

| Variable | Uso |
|---|---|
| `ALUNA_DATABASE_URL` | Base de la entidad (obligatoria para `api`, `worker`, `migrate`, `entity create` y `audit verify`): `migrate` y `entity create` con el dueño de las tablas, el resto con el rol `aluna_app` |
| `ALUNA_OIDC_ISSUER` | Emisor de los tokens (obligatoria para `api`); se descubre al arrancar |
| `ALUNA_OIDC_AUDIENCE` | Audiencia que deben traer los tokens (obligatoria para `api`) |
| `ALUNA_AUDIT_ANCHOR_DIR` | Directorio, ya existente, del ancla de los bloques de auditoría (obligatoria para `worker` y `audit verify`) |
| `ALUNA_HTTP_ADDR` | Dirección de la API; por defecto `:8080` |
| `ALUNA_RAIL_ADDR` | Dirección de las notificaciones de los rails (`api`); por defecto `:8081` |
| `ALUNA_SIMBREB_SECRET` | Secreto, de al menos 32 bytes, con que el rail simulado firma sus notificaciones (obligatoria para `api` y `worker`, el mismo en los dos) |
| `ALUNA_SIMBREB_NOTIFY_URL` | A dónde notifica el rail simulado (`worker`): las notificaciones de `api`; por defecto `http://localhost:8081` |
| `ALUNA_GMF_MODALITY` | Modalidad de la exención del GMF de la entidad (`api` y `worker`): `single_account`, por defecto, para una sola cuenta exenta por titular, o `split_accounts`, para repartir los 350 UVT del mes entre varias |
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
