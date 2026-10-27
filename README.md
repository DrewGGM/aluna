# Aluna

Core bancario en Go para entidades financieras colombianas: ledger de doble partida inmutable,
depósitos, comisiones y GMF, pagos, reconciliación y auditoría, con operación continua 24/7.

> **Estado:** en construcción. Hoy existe el binario base; los módulos llegan por etapas (ver
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
- **Un binario, varios procesos**: `aluna api` y `aluna migrate` (M0) y `aluna worker` (M2) serán
  subcomandos del mismo ejecutable. Hoy existe `aluna version`.

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
- Docker, para PostgreSQL local y las pruebas de integración (desde el ledger en adelante)

## Uso

```bash
go build -o aluna ./cmd/aluna
./aluna version
```

La versión se inyecta al compilar:

```bash
go build -ldflags "-X main.version=0.1.0" -o aluna ./cmd/aluna
```

## Pruebas

```bash
go test -race -count=1 ./...
```

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
