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
| `AsyncBalancesLagging` | page | [Cola River atascada](#cola-river-atascada) o una transacción larga del clúster |
| `ScreeningListStale` | page | [El screening contra las listas](#el-screening-contra-las-listas) |
| `HoldsNotExpiring` | page | [Cola River atascada](#cola-river-atascada) o un hold que no se puede expirar |
| `EventsUnpublished` | ticket | [Cola River atascada](#cola-river-atascada): un `outbox.publish` que falla o se descartó |
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

Sale con 0 si cada bloque cuadra con su ancla, y si no, nombra el primer bloque que no cuadra. Con el ancla
en S3, `audit verify` lee las mismas variables que el worker.

### El ancla de auditoría en S3

El ancla de producción es un bucket S3 con Object Lock (ADR-0017). Cada bloque es un objeto
`<prefijo>/<id con 20 dígitos>.sha256` con su hash en hexadecimal. Se crea solo si no existe
(`If-None-Match`) y queda retenido en modo compliance `ALUNA_AUDIT_ANCHOR_RETENTION_DAYS` días: durante la
retención nadie reescribe ni borra esa versión, tampoco la cuenta raíz. Object Lock protege versiones, no
llaves: encima de la versión retenida se puede poner otra o un marcador de borrado. Por eso el ancla solo se
lee si su llave tiene esa única versión; con más, `audit verify` falla con "the audit anchor was replaced or
deleted" y nombra el bloque. Eso es evidencia de manipulación: la versión original sigue retenida en el bucket.
Un marcador de borrado puesto en la llave de un bloque antes de anclarlo también la deja así: el sellado se
detiene en ese bloque hasta que se investigue, sin perder registros.

1. Crea el bucket con Object Lock activado (solo se puede al crearlo) y sin reglas de ciclo de vida que
   expiren versiones:

   ```bash
   aws s3api create-bucket --bucket <bucket> --object-lock-enabled-for-bucket --region <región> \
     --create-bucket-configuration LocationConstraint=<región>
   ```

2. Dale al usuario del worker, y al de `audit verify`, solo:
   - `s3:PutObject` y `s3:PutObjectRetention` sobre `arn:aws:s3:::<bucket>/<prefijo>/*` (verify no los
     necesita);
   - `s3:GetObject` y `s3:GetObjectVersion` sobre `arn:aws:s3:::<bucket>/<prefijo>/*`: el ancla se lee por el
     id de su única versión;
   - `s3:ListBucketVersions` sobre `arn:aws:s3:::<bucket>`, con `"StringLike": {"s3:prefix": "<prefijo>/*"}`
     (sin prefijo, sin la condición): el ancla lista con la llave completa como prefijo. Sin este permiso,
     `audit verify` no distingue un bloque sin ancla de uno reemplazado y falla.
3. Ponle al bucket una política que niegue a todos `s3:PutObject` sin la condición `s3:if-none-match`
   (`"Null": {"s3:if-none-match": "true"}`), `s3:DeleteObject` y `s3:DeleteObjectVersion`, y cualquier
   solicitud sin TLS (`"Bool": {"aws:SecureTransport": "false"}`). Así ni las llaves del worker ponen una
   versión encima. La cuenta raíz puede cambiar la política; lo que ponga encima, `audit verify` lo detecta.
4. Configura el worker con `ALUNA_AUDIT_ANCHOR_S3_URL` (`https://`), `ALUNA_AUDIT_ANCHOR_S3_PREFIX`,
   `ALUNA_AUDIT_ANCHOR_S3_REGION` y las llaves `AWS_*`, sin `ALUNA_AUDIT_ANCHOR_DIR`. Las llaves se leen al
   arrancar: credenciales temporales (`AWS_SESSION_TOKEN`) que se renuevan exigen reiniciar el worker antes de
   que venzan. Si no, el sellado se detiene con un 403 y los registros se acumulan sin sellar (no se pierden).
5. Comprueba la retención de un ancla, fuera del proceso:

   ```bash
   aws s3api get-object-retention --bucket <bucket> --key <prefijo>/00000000000000000001.sha256
   ```

   Debe decir `"Mode": "COMPLIANCE"` y una fecha `RetainUntilDate` unos diez años adelante.

Si el worker no puede anclar (el bucket no responde o niega el permiso), el bloque queda sellado sin ancla y
el siguiente sellado lo completa antes de sellar otro: no se pierde nada, pero `audit verify` no cuadra
hasta que se ancle.

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
   compara hasta donde llegó, y el atraso se ve en `aluna_ledger_async_lag_seconds` y la alerta
   `AsyncBalancesLagging` ([Cola River atascada](#cola-river-atascada)).
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
compartida en modo `sync` (settlement, comisiones, GMF) serializa todo el tráfico que pasa por
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

- Si el pool está saturado y la base no, sube `pool_max_conns` en `ALUNA_DATABASE_URL` (en la `api`, por
  defecto, el mayor entre 4 y el número de CPU) o agrega réplicas de `api`. El pool del `worker` tiene como
  mínimo una conexión por job que puede correr a la vez, una por cola y dos para River, aunque
  `pool_max_conns` pida menos (`aluna_db_pool_max` muestra el real). Las réplicas de los dos, por su pool,
  tienen que caber en `max_connections` del servidor.
- El modo de una cuenta se fija al crearla, y una cuenta de depósito queda atada a la versión de producto
  con que se abrió. Hoy no hay forma de cambiar el modo ni la cuenta interna de las cuentas existentes: una
  versión nueva del producto que apunte a una cuenta `async` solo descarga las cuentas que se abran
  después, y la clearing y la suspense del rail salen de su código (`L.CLEARING.<RAIL>`), no del
  producto. Las cuentas internas de alto tráfico deben crearse `async` desde el principio; las de cliente
  siguen `sync`, porque validan el saldo. Las de comisiones y GMF, además, tienen que serlo: las cuentas de
  una versión que nombre una `sync` no pueden cobrar.
- Medición de referencia: [`docs/benchmarks/`](benchmarks/).

## Cola River atascada

**Síntomas.** `AuditSealLate` (un registro de auditoría espera su sello más de 5 min), `AsyncBalancesLagging`
(un asiento de una cuenta async espera al agregador más de 5 min), `HoldsNotExpiring` (un hold pendiente lleva
más de 5 min vencido), `EventsUnpublished` (un evento lleva más de 1 h sin llegar a los consumidores internos),
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
  al apagarse, y River rescata los jobs huérfanos 5 minutos después de quedar así, o al vencer su propio
  plazo si es más largo. Mientras tanto, un periódico de controles, GL, extractos o webhooks no se vuelve a
  encolar, ni al reiniciar el worker: solo corre uno a la vez. Un pago que quedó
  `sent` no se reenvía solo: concíliealo contra el extracto del rail.
- Un hold vencido que `ledger.holds.expire` no logra expirar queda primero en cada lote y frena la expiración
  de todas las entidades. El error del job (en los descartados) dice qué falló y en qué cuenta, y el hold es
  el pendiente vencido más viejo:

  ```sql
  SELECT id, legal_entity_id, account_id, amount, expires_at
  FROM ledger.holds WHERE status = 'pending' AND expires_at <= now() ORDER BY expires_at LIMIT 1;
  ```

  Corrige la causa (por ejemplo, el saldo pendiente descuadrado que viola el `CHECK` de `ledger.balances`, ver
  [Breaks de reconciliación](#breaks-de-reconciliación)); el job siguiente retoma la expiración.
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
arranca con `discover OIDC issuer` o `the OIDC issuer must be https` en el log.

**Diagnóstico.** La API descubre el emisor al arrancar y no arranca sin él, ni con uno que no sea `https`
(salvo `http` a esta máquina), ni si su `jwks_uri` no lo es; una redirección a algo que no lo sea corta el
descubrimiento o la descarga de llaves. También le pide las llaves al arrancar: si su `jwks_uri` falla,
arranca, lo registra como `fetch OIDC issuer keys` y responde 401 hasta obtenerlas. Ya en marcha, guarda las
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
`aluna_compliance_screening_circuit_transitions_total{state="open"}` que sube. Con listas restrictivas viejas
el circuito no se abre: ver [El screening contra las listas](#el-screening-contra-las-listas).

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
  tiene fondos, no está activa o pasaría un límite de su producto, queda `rejected` (`RR04`, `AM04`, `AC06` o
  `AM02`) sin retener nada. Una orden que nadie resuelve en 7 días queda `rejected` (`SCREENING_EXPIRED`);
  resolver su caso después registra la decisión y no la envía.

### El screening contra las listas

Con `ALUNA_SCREENING_VENDOR=lists` (ADR-0018), el screening no llama a un proveedor: compara al sujeto con las
listas mismas, como permite la SFC.
- **OFAC SDN y la lista consolidada de la ONU** (obligatoria, Ley 1121 de 2006) se revisan para parties y
  beneficiarios. **Los PEP** (Decreto 830 de 2021) solo para parties: un PEP pide debida diligencia al
  vincularlo, no bloquea un pago.
- **Cómo coincide un sujeto:** por un documento exacto, o por un nombre con una similitud desde
  `ALUNA_SCREENING_THRESHOLD` (Jaro-Winkler sobre las palabras sin tildes, partículas ni orden). La
  referencia del caso nombra las entradas y por qué coincidieron: `ofac_sdn:4108~0.95` por un nombre con
  esa similitud, `pep_co:row-3nd6_dj5w-askv=document` por el documento. Un PEP se nombra por el id de la
  fila de datos.gov.co (búscalo con `$where=:id='row-…'`), nunca por su documento, que no puede ir en el
  caso ni en sus eventos.
- **El servicio de identidad** es de la entidad: Aluna no guarda nombres ni documentos. La `api` lo llama con
  `GET {ALUNA_SCREENING_IDENTITY_URL}/{entidad legal}/{party|key|account}/{referencia externa}` y
  `Authorization: Bearer {ALUNA_SCREENING_IDENTITY_TOKEN}`: la entidad, porque la referencia de una party
  solo es única dentro de ella, y `key` o `account` porque una llave y una cuenta pueden escribirse igual.
  Debe responder en menos de 2 s, sin redirigir, con 200 y `{"names": ["..."], "documents": ["..."]}`, o
  con 404 si no conoce al sujeto. Los nombres pueden llevar las tildes compuestas o aparte. Los documentos
  van como números, sin tipo (`CC`, `NIT`); los puntos, guiones y espacios no cuentan. Un NIT coincide con
  y sin su dígito de verificación cuando lo lleva aparte (`16215230-1`) o, pegado, si es de empresa (10
  dígitos desde 8 o 9) o de persona (9 dígitos desde 1 a 7): una cédula de 10 dígitos nunca se recorta. Los
  documentos de un sujeto sin nombre se comparan igual. De una party responde el sistema de
  vinculación; de un beneficiario (su llave o su cuenta), el directorio del rail. Un sujeto que no conoce,
  o del que no da ningún nombre, queda como posible coincidencia con la referencia `identity:unknown`.
- **El worker descarga las listas cada 6 horas** (`compliance.lists.refresh`) de sus dueños, o de un espejo
  con `ALUNA_SCREENING_*_URL`, y reemplaza la que cambió. No reemplaza una lista que llega vacía ni una que
  llega con menos de la mitad de sus entradas: guarda la anterior y el job falla.
- **La `api` lee las listas al arrancar y cada minuto.** Si no puede leerlas al arrancar, no arranca; si
  falla una lectura posterior, lo registra en el log `reload the restrictive lists` y sigue con las que
  tenía hasta que pasan de su antigüedad máxima.
- **Con `ALUNA_SCREENING_VENDOR=fake`**, el valor por defecto, la `api` arranca con el log `screening with the
  fake vendor`: ese proveedor despeja a casi todos y no sirve en producción.

**Síntomas propios.** Todo screening queda pendiente con el caso sin referencia del proveedor: las listas
faltan o pasaron de su antigüedad máxima, o el servicio de identidad no responde. Muchos casos con la
referencia `identity:unknown` señalan sujetos que el servicio de identidad no conoce.

**Diagnóstico.** `aluna_compliance_list_age_seconds{list}` da la antigüedad de cada lista según la base, y
`ScreeningListStale` pagina a las 36 h, antes de las 48 h en que, por defecto, el screening la rechaza: si se
baja `ALUNA_SCREENING_MAX_LIST_AGE_HOURS`, hay que bajar ese umbral. Una lista que nunca se cargó no tiene serie,
y la `api` juzga la antigüedad con lo que leyó en su última relectura. Como dueño de las tablas:

```sql
SELECT list, version, entries, refreshed_at, now() - refreshed_at AS age FROM compliance.lists;
```

Una lista con más de `ALUNA_SCREENING_MAX_LIST_AGE_HOURS` horas (48 por defecto), o que falta, hace fallar
cerrado todo screening que la revisa: la de PEP, solo el de parties, y sin abrir el circuito del proveedor,
así que los pagos siguen. El error del job `compliance.lists.refresh` dice qué dueño no respondió o qué
lista llegó vacía o encogida (ver [Cola River atascada](#cola-river-atascada)). Los casos de la última hora,
por referencia:

```sql
SELECT coalesce(vendor_ref, '(sin respuesta)') AS ref, count(*) FROM compliance.cases
WHERE created_at > now() - interval '1 hour' GROUP BY 1 ORDER BY 2 DESC;
```

Si los casos quedan sin respuesta con las listas al día en la base, busca primero el log `reload the
restrictive lists` de la `api`: si aparece, la `api` no logra releerlas (permisos, pool, consultas lentas)
y sus listas en memoria envejecen aunque la base esté al día; reiniciarla las vuelve a cargar o, si no
puede, la deja sin arrancar con el error a la vista. Si no aparece, el que falla es el servicio de
identidad: revisa sus logs y que responda en menos de 2 s.

**Mitigación.**
- Si un dueño no responde, el job lo reintenta en 6 horas y las otras listas se refrescan igual. Mientras
  tanto, apunta su `ALUNA_SCREENING_*_URL` a un espejo interno con el mismo formato.
- Si el servicio de identidad no conoce a un sujeto, la party o el pago quedan pendientes: revisa que su
  referencia externa exista en el sistema de vinculación.
- Ajustar el umbral cambia cuántos homónimos llegan a cumplimiento. Bájalo solo con cumplimiento, nunca para
  despejar casos.

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
     WORM nueva (otro prefijo del bucket, o otro bucket con Object Lock) las anclas de los bloques 1 al
     último restaurado (`<id con 20 dígitos>.sha256`) y apunta los workers a ella:
     `ALUNA_AUDIT_ANCHOR_S3_PREFIX` u `ALUNA_AUDIT_ANCHOR_S3_URL` (o `ALUNA_AUDIT_ANCHOR_DIR`). La
     generación vieja queda como evidencia. La vieja guarda las anclas de los bloques perdidos, y un ancla
     WORM no se puede quitar: si la secuencia de bloques salta adelante (lo usual), `audit verify` los lee
     como bloques anclados y borrados; si no salta, la base reusa un id ya anclado y el sellado se detiene.
     En los dos casos la cadena se rompe. Por eso el worker no arranca si el ancla tiene un bloque entre los
     33 ids siguientes al último de la base: registra "the audit anchor does not follow the database" y sale
     con 1 hasta que apunte a la generación nueva. Un ancla que no responde al arrancar solo deja un aviso: el
     worker arranca, pero no sella nada hasta que puede hacer esa misma revisión, y no sella sobre un ancla
     adelantada.
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
el contador (`pg_resetwal -e` y `-x`) es irreversible, lo hace solo un DBA y no está ensayado. Tras un
corte por replicación lógica, abre tráfico cinco minutos después del cambio: el ledger y el historial de
estados de deposits distinguen sus filas de las replicadas con el mismo número de transacción por su hora,
con ese margen (`0046_clock_margin.sql`).
