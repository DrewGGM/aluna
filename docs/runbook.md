# Runbook de operación

Qué hacer cuando Aluna da síntomas en producción: cómo se ve cada falla, cómo se diagnostica y cómo se
mitiga sin mover dinero de más. Las alertas están en
[`deploy/observability/alerts.yaml`](../deploy/observability/alerts.yaml) y las SLO en
[`slo.yaml`](../deploy/observability/slo.yaml). Aquí solo hay síntomas y procedimientos: los procesos, su
configuración, las métricas y el despliegue (imagen, PostgreSQL, migraciones) están en
[`arquitectura.md`](arquitectura.md).

Dos reglas valen para todo el runbook:

- **Nada asentado se edita.** Un error en el ledger se corrige con una reversa o un asiento de ajuste
  por la API, nunca con `UPDATE` o `DELETE`: la base lo impide con triggers y la auditoría lo
  detectaría.
- **Ante la duda, el dinero no se mueve.** Screening, IAM y rail fallan cerrados. Un incidente no se
  mitiga abriendo esas puertas.

## Señales

| Alerta | Severidad | Sección |
|---|---|---|
| `AvailabilityBudgetBurningFast` / `Slowly` | page / ticket | 5xx en endpoints de dinero: [cola atascada](#cola-river-atascada), [IAM](#falla-del-iam) o la base |
| `PostingLatencyHigh` | page | [Hot accounts lentas](#hot-accounts-lentas) |
| `ReconciliationBreakOpen` | page | [Breaks de reconciliación](#breaks-de-reconciliación) |
| `AuditSealLate` | page | [Cola River atascada](#cola-river-atascada) |
| `OperationsMetricsMissing` | page | ningún proceso lee el estado de la base: base caída o procesos sin exportar |
| `AuthorizationFailuresBurst` | page | [Falla del IAM](#falla-del-iam) o un cliente con tokens vencidos |
| `MakerCheckerApproved` | info | registro de aprobaciones, no requiere acción |

`OperationsMetricsMissing` salta si durante 10 min ningún proceso reporta las métricas de la base. Cada
proceso las lee por su cuenta: si la lectura falla, ese proceso sigue exportando sus métricas HTTP y del
pool, pero no las de la base.

La falla del screening no tiene alerta propia: se ve en la métrica de su circuito y en las parties y
pagos que quedan pendientes ([Falla del screening](#falla-del-screening)).

## Verificar la cadena de auditoría

Varios procedimientos piden verificar la auditoría. `aluna audit verify` exige el rango de bloques; el
último sellado se lee como dueño de las tablas:

```sql
SELECT max(id) FROM audit.blocks;
```

```bash
ALUNA_DATABASE_URL=… ALUNA_AUDIT_ANCHOR_DIR=… aluna audit verify --from 1 --to <último bloque>
```

Sale con 0 si cada bloque cuadra con su ancla, y si no, nombra el primer bloque que no cuadra.

## Breaks de reconciliación

**Síntomas.** `ReconciliationBreakOpen`, un evento `aluna.reconciliation.break.opened.v1` en el feed o un
cierre de periodo que falla porque hay breaks abiertos.

**Diagnóstico.** `GET /v1/reconciliation/breaks?status=open` (finanzas) lista cada break con su control,
su sujeto y la diferencia en unidades menores. Los controles corren cada 15 min:

| Control | Qué compara | Causa típica |
|---|---|---|
| `trial_balance` | Σ entries por moneda = 0 | no debería ocurrir: la base lo garantiza; tratarlo como incidente de integridad |
| `balances_match_entries` | saldo materializado = Σ entries de cada cuenta (en una `async`, hasta donde llegó el agregador) | alguien escribió `ledger.balances` a mano: incidente de integridad |
| `customers_match_control` | saldos de clientes contra el journal de esas cuentas | igual que el anterior, en cuentas de depósito |
| `rail_accounts_at_zero` | clearing y suspense del rail en cero | pagos sin liquidar, o transferencias o devoluciones en suspense |
| `accounts_mapped_to_cuif` | toda cuenta con movimiento tiene mapeo CUIF el día | una cuenta nueva sin mapeo cuando el día cerró |
| breaks de extracto | líneas del extracto del rail contra las órdenes | orden sin línea, línea sin orden, montos distintos |

**Mitigación.**

1. Si el break es de integridad (`trial_balance`, `balances_match_entries` o `customers_match_control`,
   en cuentas `sync` o `async`), para los cambios en la base y
   [verifica la cadena de auditoría](#verificar-la-cadena-de-auditoría). Si la base fue alterada, sigue
   [Restauración PITR](#restauración-pitr). Un agregador async detenido no abre estos breaks: el control
   compara hasta donde llegó, y el atraso se ve en `aluna_jobs_queue_depth{queue="ledger"}`
   ([Cola River atascada](#cola-river-atascada)).
2. Un break no se cierra solo, aunque la diferencia desaparezca: resuélvelo.
3. Si hay una diferencia real, corrígela con una reversa o un asiento de ajuste por la API, y resuelve
   el break con su causa en `POST /v1/reconciliation/breaks/{id}/resolution`.
4. `accounts_mapped_to_cuif`: carga el mapeo (`POST /v1/cuif-mappings`, maker-checker) antes de que
   cierre el día siguiente. Un mapeo no puede empezar antes de la fecha de negocio, así que un día ya
   cerrado sin mapeo queda `unmapped`.
5. `rail_accounts_at_zero` por la suspense: cada transferencia en suspense llegó con un evento
   `aluna.payments.order.suspended.v1`, con el id de su orden y por qué no se abonó. Cuando se sabe de quién
   es, abónala con `POST /v1/payment-orders/{id}/credit` y la cuenta destino (maker-checker): el monto pasa
   de la suspense a la cuenta, que debe estar activa y dentro de los límites de su producto. Antes revisa que
   nadie la haya movido ya con un asiento manual o con la reversa de su asiento: la orden seguiría `suspended`.
   Si no es de ningún cliente, devuélvela al banco que la mandó con
   `POST /v1/payment-orders/{id}/return-to-sender` y su razón (por ejemplo `AC01`, maker-checker): el monto
   vuelve de la suspense a la clearing y el job `payments.return` la entrega al rail. Si el rail no la toma o no
   responde, el job se cancela sin reintentar (el rail pudo haberla recibido) y el extracto de ese día abre un
   `order_without_line` saliente con el id de la transferencia: confirma con el rail si la recibió. Si no la
   recibió, entrégala otra vez como dueño de las tablas, reencolando su job si River todavía lo guarda (borra los
   cancelados al día) o, si no, con uno nuevo:

   ```sql
   UPDATE infra.river_job SET state = 'available', finalized_at = NULL, max_attempts = attempt + 1, scheduled_at = now()
   WHERE kind = 'payments.return' AND args ->> 'order_id' = $1 AND state = 'cancelled';

   INSERT INTO infra.river_job (kind, queue, args, max_attempts) VALUES ('payments.return', 'payments', jsonb_build_object('order_id', $1), 25);
   ```

   Una devolución de un pago saliente que quedó en suspense no es una transferencia
   recibida (responde 409 `not_in_suspense`): se abona con un asiento manual.

Hoy nada liquida la cuenta clearing del rail: el primer pago saliente abre `rail_accounts_at_zero` en
cuanto cierra su día y lo reabre en cada corrida hasta que se liquide. Todo break abierto de la entidad
bloquea el cierre de periodo. Los días cierran con el rollover de la fecha de negocio
(`POST /v1/business-date/rollover`), que el programador de tareas del operador llama con un cliente `system`
al final de cada día hábil.

## Hot accounts lentas

**Síntomas.** `PostingLatencyHigh` (p99 de `POST /v1/transactions` sobre 200 ms durante 10 min), 5xx
por `lock_timeout` (2 s) y un `aluna_db_transaction_retries_total` que crece con
`db_response_status_code="40P01"` o `"40001"`.

**Diagnóstico.** Un posting bloquea con `FOR UPDATE` solo las cuentas `sync` que toca. Una cuenta interna
compartida en modo `sync` (settlement, clearing, comisiones, GMF) serializa todo el tráfico que pasa por
ella:

```sql
-- quién espera a quién: muchas esperas Lock/transactionid detrás de pocos pids es una fila caliente
SELECT pid, pg_blocking_pids(pid) AS blocked_by, wait_event, now() - xact_start AS in_tx, left(query, 80)
FROM pg_stat_activity WHERE wait_event_type = 'Lock' ORDER BY in_tx DESC;

-- las cuentas sync con más movimientos entre las últimas 50.000 entries (por la llave primaria, sin
-- recorrer el journal)
SELECT a.code, count(*) AS entries
FROM ledger.entries e JOIN ledger.accounts a ON a.id = e.account_id
WHERE e.id > (SELECT max(id) - 50000 FROM ledger.entries) AND a.balance_mode = 'sync'
GROUP BY 1 ORDER BY 2 DESC LIMIT 10;
```

Una cuenta interna en lo alto de esa lista es la candidata. Revisa también el pool
(`aluna_db_pool_connections{state="acquired"}` contra `aluna_db_pool_max`): si está lleno y la base no
espera locks, el cuello es el pool.

**Mitigación.**

- Si el pool está saturado y la base no, sube `pool_max_conns` en `ALUNA_DATABASE_URL` (por defecto, el
  mayor entre 4 y el número de CPU) o agrega réplicas de `api`, dentro de lo que admite
  `max_connections` del servidor.
- El modo de una cuenta se fija al crearla, y una cuenta de depósito queda atada a la versión de producto
  con que se abrió. Hoy no hay forma de cambiar el modo ni la cuenta interna de las cuentas existentes: una
  versión nueva del producto que apunte a una cuenta `async` solo descarga las cuentas que se abran
  después, y la clearing y la suspense del rail salen de su código (`L.CLEARING.<RAIL>`), no del
  producto. Las cuentas internas de alto tráfico deben crearse `async` desde el principio; las de cliente
  siguen `sync`, porque validan el saldo.
- Medición de referencia: [`docs/benchmarks/`](benchmarks/).

## Cola River atascada

**Síntomas.** `AuditSealLate` (un registro de auditoría espera su sello más de 5 min),
`aluna_jobs_queue_depth` que crece en una cola, saldos async atrasados, webhooks o extractos que no salen,
pagos que se quedan `sent`.

**Diagnóstico.** Las colas son `default` (sello de auditoría, purgas, extractos, outbox), `ledger`
(holds y saldos async), `interest`, `controls`, `gl`, `webhooks` y `payments`. Los jobs periódicos solo
los encola el líder.

```sql
-- lo que espera y lo que corre, por cola
SELECT queue, kind, state, count(*), min(scheduled_at) AS oldest
FROM infra.river_job
WHERE state IN ('available', 'scheduled', 'retryable', 'running')
GROUP BY 1, 2, 3 ORDER BY oldest;

-- quién es el líder y hasta cuándo
SELECT leader_id, elected_at, expires_at FROM infra.river_leader;

-- jobs descartados: agotaron sus intentos y nadie los reintenta
SELECT kind, count(*), max(finalized_at), max(errors[array_length(errors, 1)] ->> 'error')
FROM infra.river_job WHERE state = 'discarded' AND finalized_at > now() - interval '1 day'
GROUP BY 1;
```

**Mitigación.**

- Sin worker vivo, o sin líder (`expires_at` en el pasado): reinicia el worker. Varios workers eligen un
  líder solos.
- Un job `running` que no termina (por ejemplo un rail que no responde): el worker cancela por contexto
  al apagarse, y River rescata los jobs huérfanos al cabo de su plazo de rescate. Un pago que quedó
  `sent` no se reenvía solo: concíliealo contra el extracto del rail.
- Un `outbox.publish` descartado deja sin entregar su evento a los consumidores internos (los
  acumulados de efectivo para la UIAF). El evento no se purga mientras no se publique, pero River borra
  el job descartado a los 7 días. Dentro de ese plazo, reencólalo con `JobRetry` del cliente de River o,
  como dueño de las tablas:

  ```sql
  UPDATE infra.river_job
  SET state = 'available', finalized_at = NULL, max_attempts = attempt + 1, scheduled_at = now()
  WHERE id = $1;
  ```

  Pasados los 7 días, el job ya no existe: encola uno nuevo para cada evento sin publicar (los de
  `infra.outbox` con `published_at` nulo), como dueño de las tablas, con la misma cola y los mismos
  intentos que le da el outbox:

  ```sql
  INSERT INTO infra.river_job (kind, queue, args, max_attempts)
  VALUES ('outbox.publish', 'default', jsonb_build_object('event_id', $1::text), 25);
  ```

- Una transacción larga del clúster frena la cola `ledger` y el GL (ver
  [Configuración de PostgreSQL](arquitectura.md#configuración-de-postgresql)).

## Falla del IAM

**Síntomas.** `AuthorizationFailuresBurst`, 401 `unauthenticated` en todos los canales, una `api` que no
arranca con `discover OIDC issuer` en el log.

**Diagnóstico.** La API descubre el emisor al arrancar y no arranca sin él. Ya en marcha, guarda las
llaves del emisor 10 min y las renueva en segundo plano desde la mitad de ese tiempo; un token con una
llave desconocida vuelve a pedirlas, a lo sumo una vez cada 30 s. Con el IAM caído, la API sigue
validando con las llaves que tiene hasta que vencen, y después responde 401 a todo: falla cerrada.

- `curl $ALUNA_OIDC_ISSUER/.well-known/openid-configuration` desde la red de la API.
- Si el emisor responde: compara `iss` y `aud` de un token rechazado con `ALUNA_OIDC_ISSUER` y
  `ALUNA_OIDC_AUDIENCE`, y la hora del nodo (un `iat` adelantado más de 1 min se rechaza). Un token vive
  como mucho 15 min.
- Un 401 solo de un cliente es casi siempre su token, no el IAM.

**Mitigación.** Restablece el IAM. No reinicies las `api` mientras el emisor esté caído: perderían las
llaves que todavía tienen y no volverían a arrancar. No hay modo degradado sin autenticación.

## Falla del screening

**Síntomas.** Parties que quedan `pending_screening` al registrarse, órdenes de pago `pending_screening`,
`aluna_compliance_screening_circuit_transitions_total{state="open"}` que sube.

**Diagnóstico.** El screening falla cerrado: un error del proveedor, una respuesta que tarda más de 2 s o
el circuito abierto dejan el caso pendiente, nunca limpio. El circuito se abre tras 5 fallas seguidas, no
llama al proveedor durante 30 s y luego deja pasar una consulta de prueba. Los casos llegan como evento
`aluna.compliance.screening.pending.v1`.

**Mitigación.**

- Restablece el proveedor. El circuito se cierra solo con la primera consulta de prueba que responda.
- Los casos que quedaron pendientes durante la caída se listan con
  `GET /v1/screening-cases?status=pending` y los resuelve cumplimiento con
  `POST /v1/screening-cases/{id}/resolution` (maker-checker). Una party resuelta sale de
  `pending_screening`. Una orden de pago despejada retiene sus fondos y sale a enviarse: con el GMF del
  momento y, si nombró una cotización, la comisión cotizada o la menor entre esa y la vigente, según
  `ALUNA_QUOTE_POLICY`; sin cotización, la comisión vigente. Una confirmada, o una despejada cuya cuenta ya no
  tiene fondos, no está activa o pasaría un límite de su producto, queda `rejected` (`RR04`, `AM04`, `AC06` o `AM02`) sin retener nada. Una orden que nadie
  resuelve en 7 días queda `rejected` (`SCREENING_EXPIRED`); resolver su caso después registra la decisión y
  no la envía.

## Restauración PITR

Para una corrupción lógica (un error humano, un despliegue que escribió mal) se restaura el clúster de la
entidad a un instante anterior al error (RPO: el instante elegido, RTO < 4 h). El ensayo automático está
en [`test/restore/restore_drill_test.go`](../test/restore/restore_drill_test.go) y hace estos mismos
pasos:

```bash
go test -count=1 -tags=restore -timeout=30m ./test/restore/...
```

**Requisitos.** El primario archiva su WAL (`wal_level=replica`, `archive_mode=on`, un
`archive_command` que copia a almacenamiento WORM) y hay un `pg_basebackup` reciente. Un backup que nunca
se restauró no cuenta: el ensayo corre cada trimestre.

**Procedimiento.**

1. **Detén la escritura.** Saca las `api` de rotación y apaga los workers. Anota el instante del error
   con su zona horaria: [la verificación de la auditoría](#verificar-la-cadena-de-auditoría) nombra el
   primer bloque que no cuadra, y su `sealed_at` y los logs acotan el instante.
2. **Archiva lo último.** Si el primario vive, `SELECT pg_switch_wal()` y espera a que
   `pg_stat_archiver.last_archived_wal` llegue a ese segmento.
3. **Restaura el backup** en un clúster nuevo, sin tocar el original: copia el `pg_basebackup` al nuevo
   `PGDATA` y crea `recovery.signal`.
4. **Recupera hasta el instante.** Arranca el clúster nuevo con estos parámetros y espera a que
   `SELECT pg_is_in_recovery()` devuelva `false`:

   ```ini
   restore_command = 'cp /wal/%f %p'
   recovery_target_time = '2026-03-02 14:05:00-05'
   recovery_target_action = 'promote'
   ```

5. **Verifica antes de abrir tráfico.**
   - [Verifica la cadena de auditoría](#verificar-la-cadena-de-auditoría) hasta el último bloque
     restaurado, con el ancla WORM: debe cuadrar. El ancla conserva los hashes de los bloques sellados
     después del instante, que la base restaurada ya no tiene: no se verifican, y sus comandos se
     perdieron con la restauración.
   - **Abre una generación nueva del ancla** antes de que cualquier worker selle: copia a una ubicación
     WORM nueva las anclas de los bloques 1 al último restaurado (`<id con 20 dígitos>.sha256`) y apunta
     `ALUNA_AUDIT_ANCHOR_DIR` de los workers a ella. La generación vieja queda como evidencia. La vieja
     guarda las anclas de los bloques perdidos, y un ancla WORM no se puede quitar: si la secuencia de
     bloques salta adelante (lo usual), `audit verify` los lee como bloques anclados y borrados; si no
     salta, la base reusa un id ya anclado y el sellado se detiene. En los dos casos la cadena se rompe.
   - Antes de arrancar un worker, revisa los pagos que esperan salir: la base restaurada no sabe qué
     recibió el rail después del instante, y el worker los enviaría otra vez. Lo mismo vale para las
     devoluciones al banco de origen que esperan salir (`payments.return`). Concilia cada orden contra el
     extracto del rail y cancela, como dueño de las tablas, el envío de las que el rail ya recibió:

     ```sql
     SELECT id, kind, args, state, scheduled_at FROM infra.river_job
     WHERE kind IN ('payments.send', 'payments.return') AND state IN ('available', 'scheduled', 'retryable');

     UPDATE infra.river_job SET state = 'cancelled', finalized_at = now()
     WHERE id = $1 AND state IN ('available', 'scheduled', 'retryable');
     ```

   - Corre los controles (arranca el worker o espera su corrida) y revisa
     `GET /v1/reconciliation/breaks?status=open`.
   - Conserva `pg_controldata` y el log de la recuperación como evidencia del incidente.
6. **Abre tráfico** apuntando `ALUNA_DATABASE_URL` al clúster restaurado. Lo que se perdió después del
   instante (pagos del rail, comandos de canales) se reconstruye con los extractos del rail y los logs de
   los canales, no a mano en la base.

**Integradores.** El cursor de `GET /v1/events` lleva el contador de transacciones del clúster. Una PITR
lo devuelve al instante restaurado, igual que promover una réplica asíncrona que no tenía lo último: un
integrador que leyó eventos de después del instante tiene un cursor por delante del contador, y se
saltaría en silencio los eventos nuevos. Antes de abrir tráfico, avisa a los integradores del instante:
que relean desde un cursor anterior a él (o desde el principio) deduplicando por id de evento, y que los
eventos posteriores que recibieron ya no existen. Los webhooks no tienen este problema: su posición vive
en la base restaurada. Un `pg_dump`/restore o una replicación lógica tampoco conservan el contador, y el
feed queda vacío para todos hasta que el contador nuevo pase al viejo: el remedio es el mismo. Adelantar
el contador (`pg_resetwal -e` y `-x`) es irreversible, lo hace solo un DBA y no está ensayado.
