# bionicpro-reports

Go/Gin API for daily patient telemetry reports. CSV files are stored in a private S3-compatible bucket (MinIO/Ceph); the API returns a signed CDN URL and Nginx reverse-proxies and caches the file.

## Request flow

1. `GET /report?date=YYYY-MM-DD` validates and rotates the `session_id` through `bionicpro-auth`.
2. The API verifies the Keycloak access token signature, issuer, expiry, optional audience and the `prosthetic_user` role.
3. The authenticated username from the trusted auth response is used to query `patient_identity_id` in ClickHouse. The client cannot provide a patient identity or object key.
4. The API reads the S3 manifest for this patient and date. If it is younger than `REPORT_CACHE_TTL` and the referenced CSV object exists, the existing report is reused.
5. Otherwise, the API generates CSV, uploads it to MinIO and updates the manifest.
6. The API returns JSON containing a signed URL under `CDN_BASE_URL`. The browser downloads the CSV from Nginx, not from the API endpoint.
7. Nginx calls the API's signature-validation subrequest on every request (including cache hits), then serves the immutable object from its disk cache or fetches it from the API's S3-backed origin handler.

Example response:

```json
{
  "date": "2026-11-25",
  "url": "http://192.168.56.106:8083/reports/...?...",
  "format": "csv",
  "cached": true,
  "generated_at": "2026-11-25T19:00:00Z"
}
```

`cached` means the API reused an already generated S3 object. It does not describe the Nginx cache state; inspect `X-Cache-Status` on the CDN response for `HIT`/`MISS`.

## S3 key layout and refresh strategy

Patient identity is SHA-256 hashed before it is used in an object key, so usernames and Keycloak identifiers are not exposed in object paths.

```text
reports/
  {sha256(patient_identity)}/
    {yyyy}/{mm}/{dd}/
      latest.json
      {sha256(csv-content)}.csv
```

`latest.json` points to the current immutable CSV object and records its generation time. The API reuses it for `REPORT_CACHE_TTL` (default `24h`). When it expires, the report is regenerated and uploaded under a content-hash filename; then the manifest is atomically replaced with the new version. Old CSV objects are not overwritten, so their CDN URLs remain valid and cached responses cannot accidentally serve stale bytes under a new version URL. Old object cleanup can be added later as a lifecycle policy (for example, retain versions for 30 days).

Nginx caches successful CSV responses for up to seven days. Its cache key is the object path and deliberately excludes the signed query parameters. `auth_request` validates the signature before the cache lookup, so a cached file is not publicly accessible without a valid, unexpired signature. A newly generated report has a different content-hash URL, which naturally invalidates the old CDN URL without a purge operation.

## Configuration

The Compose deployment uses `bionicpro-reports/.env`.

| Variable | Default / example | Description |
|---|---|---|
| `PORT` | `8082` | API listen port |
| `OLAP_CLICKHOUSE_URL` | — | ClickHouse native-protocol DSN |
| `AUTH_INTERNAL_URL` | `http://bionicpro-auth:8081` | Internal auth service URL |
| `AUTH_INTERNAL_SECRET` | — | Must match the auth service internal secret; at least 32 bytes |
| `OIDC_ISSUER` | Keycloak realm issuer | Exact `iss` claim |
| `OIDC_AUDIENCE` | `broker` in the supplied environment | Optional expected audience. The current Keycloak token has `aud: ["broker", "account"]`; ideally add a dedicated `bionicpro-reports` audience mapper later. |
| `S3_ENDPOINT` | `http://minio:9000` | Internal S3 endpoint; for Ceph use its S3 endpoint |
| `S3_REGION` | `us-east-1` | S3 signing region |
| `S3_ACCESS_KEY` | `minio_user` | MinIO/Ceph access key |
| `S3_SECRET_KEY` | — | MinIO/Ceph secret key |
| `S3_BUCKET` | `reports` | Private bucket; created by the API if missing |
| `CDN_BASE_URL` | `http://192.168.56.106:8083` | URL reachable by the browser, not a Docker-only hostname |
| `CDN_SIGNING_SECRET` | — | HMAC key for signed CDN URLs; at least 32 bytes |
| `CDN_URL_TTL` | `24h` | Signed link lifetime |
| `REPORT_CACHE_TTL` | `24h` | How long the S3 manifest is considered fresh |

Do not use the example MinIO credentials in a real deployment. Use dedicated least-privilege S3 credentials and keep the bucket private. The supplied Compose configuration is for local development.

## Docker Compose

The project Compose file starts MinIO and the `reports-cdn` Nginx service together with the API.

- MinIO is available inside the Compose network at `http://minio:9000`.
- MinIO's host API port is `9002` because ClickHouse already publishes host port `9000`.
- MinIO console is available at `http://localhost:9001`.
- CDN is available at `http://192.168.56.106:8083` (change `CDN_BASE_URL` to the address your browser uses).
- The API creates the `reports` bucket on startup, retrying while MinIO starts.

Start the stack from the repository root:

```bash
docker compose up -d --build
docker compose logs -f bionicpro-reports reports-cdn minio
```

## API endpoints

- `GET /health` — liveness check.
- `GET /report/days` — authenticated list of dates with source data.
- `GET /report?date=YYYY-MM-DD` — authenticated JSON response with a signed CDN URL; generates the report if the S3 manifest is missing or expired.
- `GET /reports/{object-key}?expires=...&signature=...` — S3-backed origin endpoint. Intended to be accessed through Nginx; signature is also validated by the handler itself.
- `GET /internal/cdn-auth` — Nginx `auth_request` endpoint; validates a signed URL. It should not be exposed publicly by a separate ingress.

HTTP behavior for `/report`:

- `200` — JSON report URL.
- `400` — missing or invalid date.
- `401` — missing/invalid/expired session or invalid access token.
- `403` — authenticated user lacks `prosthetic_user`.
- `500` — report generation or S3 manifest update failed.

`/report/days` remains `Cache-Control: no-store` and still rotates the browser session as before.

## Notes

- `patient_identity_id` in ClickHouse stores the username (for example, `john.doe`), so the report query uses the trusted `username` returned by `bionicpro-auth`, not the Keycloak `sub` UUID.
- The CDN URL is signed and time-limited. Do not make the MinIO bucket public.
- `reports-cdn` is a cache layer, not a data store. MinIO remains the source of truth for report objects.
