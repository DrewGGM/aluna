# Colecciones de Postman

`aluna.postman_collection.json` tiene una petición por cada operación de
[`api/openapi.yaml`](../../api/openapi.yaml), agrupadas en una carpeta por recurso (el primer segmento
después de `/v1`). `aluna-dev.postman_environment.json` es el entorno de desarrollo. Los dos se generan
desde el contrato: no se editan a mano.

## Uso

1. En Postman, *Import* de los dos archivos y selecciona el entorno **Aluna dev**.
2. Levanta PostgreSQL, Keycloak, la API y el worker como dice el [inicio rápido](../../README.md#uso).
3. Envía cualquier petición.

La colección no tiene aserciones: un 4xx de la API es una respuesta, no un fallo, también al correrla
con newman.

El entorno apunta a la API en `http://localhost:8080` y al emisor `http://localhost:8180/realms/aluna`,
con el cliente `aluna-dev` y el usuario de prueba `operator`/`operator` del realm de desarrollo. Para
otro usuario o servidor, cambia `username`, `password`, `base_url` u `oidc_issuer` en el entorno.

## Token

Antes de cada petición, el script de la colección pide un token a Keycloak con el *password grant*
(como el `curl` del README) y lo guarda en `access_token` hasta 30 s antes de que venza; las peticiones
lo envían como `Authorization: Bearer {{access_token}}`. Para forzar uno nuevo, borra `access_token`
del entorno. Si Keycloak no responde, el error sale en la consola de Postman y la API contesta 401.

## Idempotency-Key

Las operaciones que declaran el header `Idempotency-Key` tienen un script que pone un UUID nuevo en
`idempotency_key` en cada envío, así que cada envío es un comando distinto. Para reintentar con la
misma clave, comenta la línea de ese script (pestaña *Scripts*, *Pre-request*, de la petición) y
reenvía: el valor anterior sigue en el entorno.

## Ejemplos

Cada cuerpo y parámetro toma el `example` del contrato si lo tiene (un asiento balanceado, una
definición de producto válida, un archivo CUIF, un monto `{"amount": 10000000, "currency": "COP"}`).
Si no, lleva solo las propiedades obligatorias con valores de relleno: UUID en ceros, fechas
`2026-01-02` y el primer valor de cada enum. Reemplaza los ids por los de recursos que existan.

## Regenerar

Después de cambiar el contrato:

```bash
go generate ./api/postman
```

`go test ./api/postman` falla si a la colección le falta una operación del contrato o si los archivos
de esta carpeta no son los que produce el generador, y el job `generated` del CI falla si quedaron
sin regenerar.
