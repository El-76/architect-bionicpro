# bionicpro-reports

Small Go/Gin service exposing `GET /report` (daily CSV export) and `GET /report/days` (available report dates).

## Request flow

1. Read the `session_id` HttpOnly cookie from the incoming request.
2. Call `POST {AUTH_INTERNAL_URL}/internal/session/validate-and-rotate`, forwarding that cookie and sending `X-Internal-Auth: $AUTH_INTERNAL_SECRET`.
3. Forward the `session_id` `Set-Cookie` header returned by `bionicpro-auth` to the browser.
4. Verify the returned Keycloak access token (RS256 signature via the realm JWKS, issuer, audience, expiration and optional `nbf`).
5. Require `prosthetic_user` in either `realm_access.roles` or a `resource_access` role list.
6. Query `olap.mart_patient_telemetry_daily` using `patient_identity_id = user_id` from the trusted auth response.
7. For `GET /report`, additionally filter by the requested `date` and stream the selected columns as CSV. For `GET /report/days`, return the distinct dates available for that patient as JSON.

**Important integration assumption:** this implementation assumes the Keycloak `sub` (`user_id` returned by `bionicpro-auth`) is exactly the value stored in `patient_identity_id`. If the warehouse uses a different identity key (for example, a Yandex ID), add an explicit, trusted identity mapping; do not accept a patient ID from a request parameter.

The auth service's current internal endpoint performs session rotation and returns the access token in one operation. Therefore role verification necessarily happens immediately after that call; if the role is missing, the request returns `403` but the newly rotated cookie is still forwarded to the browser.

## Configuration

| Variable | Required | Default | Description |
|---|---:|---|---|
| `OLAP_CLICKHOUSE_URL` | yes | — | ClickHouse native-protocol DSN; use a read-only account |
| `AUTH_INTERNAL_SECRET` | yes | — | Same secret as in `bionicpro-auth`; at least 32 bytes |
| `AUTH_INTERNAL_URL` | no | `http://bionicpro-auth:8081` | Internal base URL of `bionicpro-auth` |
| `OIDC_ISSUER` | no | `http://192.168.56.106:8080/realms/reports-realm` | Exact Keycloak realm issuer URL |
| `OIDC_AUDIENCE` | no | `bionicpro-auth` | Expected `aud` claim in the access token; adjust to the actual access-token audience |
| `PORT` | no | `8082` | HTTP listen port |

`AUTH_INTERNAL_SECRET` must match the value configured in `bionicpro-auth`. Do not expose the internal auth endpoint publicly. Use a private service network and TLS where traffic crosses an untrusted network.

## Run locally

```bash
export OLAP_CLICKHOUSE_URL='clickhouse://reports_reader:password@localhost:9000/olap?secure=false&compress=lz4'
export AUTH_INTERNAL_SECRET='replace-with-the-same-32+-byte-secret-as-auth'
export AUTH_INTERNAL_URL='http://localhost:8081'
go mod tidy
go run .
```

Then send the browser session cookie and choose the report date:

```bash
curl -i --cookie 'session_id=YOUR_SESSION_ID' \
  'http://localhost:8082/report?date=2026-11-23'
```

`date` is required and must be a valid `YYYY-MM-DD` calendar date. The response is `text/csv; charset=utf-8` with a date-specific `Content-Disposition` attachment header. To discover dates with data for the authenticated patient:

```bash
curl -i --cookie 'session_id=YOUR_SESSION_ID' \
  http://localhost:8082/report/days
```

The dates endpoint returns JSON in descending order, for example:

```json
{"days":["2026-11-23","2026-11-22","2026-11-20"]}
```

Both endpoints validate the session, rotate its cookie through `bionicpro-auth`, verify the access token and require `prosthetic_user`.

## Docker

```bash
docker build -t bionicpro-reports:local .
docker run --rm -p 8082:8082 \
  -e OLAP_CLICKHOUSE_URL='clickhouse://reports_reader:password@host.docker.internal:9000/olap?secure=false&compress=lz4' \
  -e AUTH_INTERNAL_URL='http://host.docker.internal:8081' \
  -e AUTH_INTERNAL_SECRET="$AUTH_INTERNAL_SECRET" \
  bionicpro-reports:local
```

## HTTP behavior

- `200` — CSV report from `GET /report`, or JSON list of available dates from `GET /report/days`.
- `400` — missing or invalid `date` parameter on `GET /report`.
- `401` — missing/invalid/expired session or invalid access token.
- `403` — authenticated user does not have `prosthetic_user`.
- `502` — auth service/JWKS dependency failure or invalid auth response.
- `500` — OLAP query/read failure.

`/health` is also provided for container health checks; it does not test downstream dependencies.

## Docker Compose

The Compose file runs only `bionicpro-reports`: it expects `bionicpro-auth` and the `olap-clickhouse` ClickHouse container to be reachable on a shared Docker network. This avoids starting a second ClickHouse instance or duplicating the auth service.

1. Copy `.env.example` to `.env` and set `OLAP_CLICKHOUSE_URL` and `AUTH_INTERNAL_SECRET`.
2. Ensure the Docker network used by the `bionicpro-auth` and `olap-clickhouse` exists and that both containers are attached to it. The default expected network name is `bionicpro-network`; override it with `DOCKER_NETWORK` if your stack uses another name.
3. Ensure the hostnames in `AUTH_INTERNAL_URL` and `OLAP_CLICKHOUSE_URL` resolve to `bionicpro-auth` and `olap-clickhouse` from that network.
4. Start the service:

```bash
docker compose up -d --build
docker compose logs -f bionicpro-reports
```

If the shared network does not exist yet, create it with `docker network create bionicpro-network` and attach the existing `bionicpro-auth` and `olap-clickhouse` to it (or declare this same network as external in their Compose projects). Do not create a separate network for reports, because container DNS names are scoped to shared networks.
