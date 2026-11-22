# architect-bionicpro

## Architecture

- `bionicpro-auth` owns the browser session and is the only service that talks to Keycloak.
- The frontend starts login through `bionicpro-auth` and keeps only the HTTP-only `session_id` cookie.
- The frontend calls `reports-api` directly (`http://192.168.56.106:8000` in the supplied local configuration).
- For every protected request, `reports-api` calls the private `bionicpro-auth` endpoint `POST /internal/session/validate-and-rotate`, forwarding the browser's `session_id` cookie and the service-to-service secret.
- `bionicpro-auth` validates the session, refreshes the access token when it expires soon, rotates the session ID, and returns the access token in the private response. It also returns a `Set-Cookie` header containing the rotated session cookie.
- `reports-api` must copy that `Set-Cookie` header to its response to the browser and use the returned access token only server-side. It must never return the access token or session ID in its JSON response.

There is intentionally no reverse proxy in `bionicpro-auth` and no `REPORT_API_URL` setting. The `reports-api` source code is not part of this archive, so its middleware must be wired using the contract below.

## Configuration

1. Copy `bionicpro-auth/.env.example` to `bionicpro-auth/.env`.
2. Set `OIDC_CLIENT_SECRET` to the current secret of the `bionicpro-auth` client in Keycloak (Clients → bionicpro-auth → Credentials).
3. Set `TOKEN_ENCRYPTION_KEY` to a long random secret. The application derives a 256-bit AES key from this value and uses AES-GCM to encrypt refresh tokens before storing them in RAM.
4. Set `AUTH_INTERNAL_SECRET` to a different long random secret. Configure the exact same value in `reports-api` as its `AUTH_INTERNAL_SECRET`.
5. Do not commit `.env` or share either secret.

Both secrets must be at least 32 bytes. For example, generate independent values with:

```bash
openssl rand -hex 32
openssl rand -hex 32
```

Then start the services included in this repository:

```bash
docker compose up --build
```

`reports-api` is external to this repository and must be started separately. It must be reachable by the browser at the URL configured in `frontend/.env`.

## reports-api integration contract

For every protected endpoint, before executing its business logic:

1. Read the incoming `session_id` cookie from the browser request.
2. Call `POST http://<bionicpro-auth-host>:8081/internal/session/validate-and-rotate`.
3. Forward the cookie unchanged in the `Cookie` header.
4. Add `X-Internal-Auth: <AUTH_INTERNAL_SECRET>`.
5. If `bionicpro-auth` responds with `401`, return `401` to the browser and forward any `Set-Cookie` header (it clears the expired cookie). If it responds with `403`, the internal secret is misconfigured; do not treat that as an anonymous user. For upstream `5xx`, return an appropriate `5xx`.
6. On `200`, parse JSON fields `user_id`, `username`, `access_token`, and `access_token_expires_at`.
7. Copy the `Set-Cookie` response header from `bionicpro-auth` to the response sent to the browser. This is how the browser receives the rotated session ID. Do not expose the raw session ID in JSON.
8. Use `access_token` only inside `reports-api` (for example, pass it to the existing authorization/business layer or downstream service). Never send it to the browser.

Example internal request:

```http
POST /internal/session/validate-and-rotate HTTP/1.1
Host: bionicpro-auth:8081
Cookie: session_id=<browser-cookie-value>
X-Internal-Auth: <shared-secret>
```

Example successful internal response:

```http
HTTP/1.1 200 OK
Set-Cookie: session_id=<new-random-id>; Path=/; Max-Age=1800; HttpOnly; SameSite=Lax
Content-Type: application/json

{
  "user_id": "<keycloak-sub>",
  "username": "user1",
  "access_token": "<access-token>",
  "access_token_expires_at": "2026-11-15T20:00:00Z"
}
```

The internal endpoint is protected by `X-Internal-Auth` and must only be reachable by trusted backend services. In a deployment with a Docker network, use the service DNS name `bionicpro-auth:8081` and keep the port off the public network if possible. The current compose file publishes port 8081 for the local frontend login flow.

Because frontend and reports-api use different ports on the same host, the browser sends the host-scoped cookie to both when `credentials: 'include'` is used. `reports-api` must enable credentialed CORS for exactly `http://192.168.56.106:3000` (not `*`) and expose `Content-Disposition` if the frontend reads it.

## Session and token behavior

- Access token lifetime in the supplied realm export: 120 seconds.
- Session idle timeout: 30 minutes.
- Session absolute lifetime: 8 hours.
- Refresh tokens are rotated by Keycloak (`Revoke Refresh Token` enabled, maximum reuse 0).
- Refresh tokens are AES-256-GCM encrypted in memory; access tokens are kept in memory.
- A protected request through `reports-api` rotates the session ID. The old ID is removed from the active session map. A short 5-second alias lets already-in-flight parallel requests using the old cookie receive the same new cookie rather than spuriously failing; after that grace period the old ID is rejected.
- `/auth/session` only checks the session for the frontend page and does not rotate it.
- Sessions and token material are in RAM and are lost on `bionicpro-auth` restart.

## Keycloak realm import caveat

Keycloak's `--import-realm` does not overwrite a realm that already exists in its database. If `reports-realm` already exists, update these settings manually in Realm Settings:

- Tokens → Access Token Lifespan: `2 minutes`.
- Sessions → SSO Session Idle: `30 minutes`.
- Sessions → SSO Session Max: `8 hours`.
- Tokens → Revoke Refresh Token: enabled; Refresh Token Max Reuse: `0`.

The realm export no longer embeds the `bionicpro-auth` client secret. For an existing realm, the current client secret remains in Keycloak; for a newly imported realm, retrieve the generated secret from the Keycloak client credentials page and put it in `.env`.

## Cookie flags

For the requested local HTTP test setup, cookie flags are intentionally unchanged: `Secure=false`, `HttpOnly=true`, `SameSite=Lax`. Do not use this configuration over an untrusted network or in production; production must use HTTPS and `Secure=true`.



echo -n "eyJhbGciOiJSUzI1NiIsInR5cCIgOiAiSldUIiwia2lkIiA6ICJnVTU1NHFnX0JkaEpyMDNabC13ZV82ZTIzdlg1ejFUdkZhQ24zTDNIbGZNIn0.eyJleHAiOjE3OTUwMzQ4MTEsImlhdCI6MTc5NTAzNDY5MSwiYXV0aF90aW1lIjoxNzk1MDM0NjkxLCJqdGkiOiI1Y2I4ZTZhNy0wOTM2LTRjNDAtOWFiMy01ZmE1MWQ3NDNkZDUiLCJpc3MiOiJodHRwOi8vMTkyLjE2OC41Ni4xMDY6ODA4MC9yZWFsbXMvcmVwb3J0cy1yZWFsbSIsImF1ZCI6ImFjY291bnQiLCJzdWIiOiJhOTI1ZTc4MS0zZmJlLTQ3MWUtYjBmZC00NjliNzRmZjMwYWIiLCJ0eXAiOiJCZWFyZXIiLCJhenAiOiJiaW9uaWNwcm8tYXV0aCIsIm5vbmNlIjoiSm9sREZESXZfOXhSUmVtZTFxQjhwcS1FdEVKN1NsZWNreDVBUExmYjBZcyIsInNlc3Npb25fc3RhdGUiOiJjMjIyNzZmYy1mZmQyLTQxMDktYTNlMC1lYWUzNTRjMzgwNjMiLCJhY3IiOiIxIiwicmVhbG1fYWNjZXNzIjp7InJvbGVzIjpbInByb3RoZXRpY191c2VyIiwib2ZmbGluZV9hY2Nlc3MiLCJkZWZhdWx0LXJvbGVzLXJlcG9ydHMtcmVhbG0iLCJ1bWFfYXV0aG9yaXphdGlvbiJdfSwicmVzb3VyY2VfYWNjZXNzIjp7ImFjY291bnQiOnsicm9sZXMiOlsibWFuYWdlLWFjY291bnQiLCJtYW5hZ2UtYWNjb3VudC1saW5rcyIsInZpZXctcHJvZmlsZSJdfX0sInNjb3BlIjoib3BlbmlkIHByb2ZpbGUgZW1haWwiLCJzaWQiOiJjMjIyNzZmYy1mZmQyLTQxMDktYTNlMC1lYWUzNTRjMzgwNjMiLCJlbWFpbF92ZXJpZmllZCI6ZmFsc2UsInByZWZlcnJlZF91c2VybmFtZSI6ImpvaG4uZG9lIiwiZW1haWwiOiJqb2huQGV4YW1wbGUuY29tIn0.HNpEpFxihYBEvp4rGtSuEhAs9DRPFh6Os1nLtatsNocpBe880MxNqcYgI-n5GFAFmWxGs9606jhbZJTyGajkpwGUEaJ_2pi9deUCiRAZL2Ipq06qNrlhv6lOWK-Z2lpvMmle1WYNJp274lifasq_B-4OFE3O9pzML5kzlFDHzfx7_aR1hCFxlxqhFmCiBvkqbyG6baVBYnTFWlJnZwGKlEEfihuBOJxKRTdLQg4CUGkRiR-HGd_UUH0jpv3rVpH6WQN8nDNXhAyaDJl8ZTl3PmCAXMeedr1GbYptRzQXRUsxRaJBMHcASieJGQ6TtcCs9o44nVQ-2XYZh_jnoqvjTA" | cut -d '.' -f2 | tr '_-' '/+' | base64 -d 2>/dev/null | jq .
{
  "exp": 1795034811,
  "iat": 1795034691,
  "auth_time": 1795034691,
  "jti": "5cb8e6a7-0936-4c40-9ab3-5fa51d743dd5",
  "iss": "http://192.168.56.106:8080/realms/reports-realm",
  "aud": "account",
  "sub": "a925e781-3fbe-471e-b0fd-469b74ff30ab",
  "typ": "Bearer",
  "azp": "bionicpro-auth",
  "nonce": "JolDFDIv_9xRReme1qB8pq-EtEJ7Sleckx5APLfb0Ys",
  "session_state": "c22276fc-ffd2-4109-a3e0-eae354c38063",
  "acr": "1",
  "realm_access": {
    "roles": [
      "prothetic_user",
      "offline_access",
      "default-roles-reports-realm",
      "uma_authorization"
    ]
  },
  "resource_access": {
    "account": {
      "roles": [
        "manage-account",
        "manage-account-links",
        "view-profile"
      ]
    }
  },
  "scope": "openid profile email",
  "sid": "c22276fc-ffd2-4109-a3e0-eae354c38063",
  "email_verified": false,
  "preferred_username": "john.doe",
  "email": "john@example.com"
}



1. Browser Flow

Authentication → Flows → Browser → Conditional 2FA => REQUIRED

Authentication → Flows → Browser → Conditional 2FA -> OTP Foem => REWUIRED

2. Новые пользователи

   Authentication → Required Actions → Configure OTP.

       Enabled => On
       Set as default action => On



1. Identiry Providers

Yandex

  Client ID
  Client Secret
  Store tokens => On


http://192.168.56.106:8080/realms/reports-realm/broker/yandex/endpoint


Настройка для bionicpro-auth
1. Открой Clients → bionicpro-auth.
2. Перейди в Client scopes.
3. Открой bionicpro-auth-dedicated.
4. Перейди в Mappers → Configure a new mapper.
5. Выбери User Session Note.
Заполни настройки:
Mapper: Identity Provider
NameIdentity Provider


User Session Noteidentity_provider

Token Claim Nameidentity_provider

Claim JSON TypeString


Add to access token
ON

Add to ID token
По желанию



Realm Roles

default-roles-reports-realm

Assign role -> broker/read token



docker exec -it bionicpro-auth-db /bin/bash

/ # psql -h 127.0.0.1 -p 5432 -U postgres
psql (16.16)
Type "help" for help.

postgres=# \d+
                                         List of relations
 Schema |     Name      | Type  |  Owner   | Persistence | Access method |    Size    | Description
--------+---------------+-------+----------+-------------+---------------+------------+-------------
 public | user_profiles | table | postgres | permanent   | heap          | 8192 bytes |
(1 row)

postgres=# select * from user_profiles;
 keycloak_user_id | username | identity_provider | email | display_name | first_name | last_name | yandex_user_id | profile_data | created                   _at | updated_at
------------------+----------+-------------------+-------+--------------+------------+-----------+----------------+--------------+--------                   ----+------------
(0 rows)

postgres=# select * from user_profiles;
           keycloak_user_id           |  username   | identity_provider |         email         |  display_name   | first_name | last_name                    | yandex_user_id |                                                                                                                                                               profile_data                                                                                                                                                               |          created_at           |          updated_at
--------------------------------------+-------------+-------------------+-----------------------+-----------------+------------+----------                   -+----------------+-----------------------------------------------------------------------------------------------------------------------                   ------------------------------------------------------------------------------------------------------------------------------------------                   -----------------------------------+-------------------------------+-------------------------------
 642caea6-ec83-4c0b-b2f4-8d06bfe53e07 | yndx.maaaah | yandex            | yndx.maaaah@yandex.ru | Anton Ostroumov | Anton      | Ostroumov                    | 389218555      | {"id": "389218555", "login": "yndx.maaaah", "psuid": "1.ABQgPA.rTYlyMioFqgIMZ8Ea93_Sw.9bhARG1s3eVIFI1hv0DVBQ", "emails                   ": ["yndx.maaaah@yandex.ru"], "birthday": "1979-08-17", "client_id": "2fc66db9b68f498a819f8d184b65de8b", "default_email": "yndx.maaaah@yan                   dex.ru", "is_avatar_empty": false} | 2026-11-21 20:10:44.842737+00 | 2026-11-21 20:10:44.842737+00
(1 row)




















II этап


maaaah@debian:~/architect-bionicpro$ docker exec -it data-postgres /bin/bash
root@4058ef4907c6:/# psql -h 127.0.0.1 -p 5432 -U data_user -d data_db
psql (16.16 (Debian 16.16-1.pgdg13+1))
Type "help" for help.

data_db=# \d+
                                                        List of relations
 Schema |                  Name                  |   Type   |   Owner   | Persistence | Access method |    Size    | Description
--------+----------------------------------------+----------+-----------+-------------+---------------+------------+-------------
 public | sensor_measurements                    | table    | data_user | permanent   | heap          | 328 kB     |
 public | sensor_measurements_measurement_id_seq | sequence | data_user | permanent   |               | 8192 bytes |
(2 rows)

data_db=# select * from sensor_measurements;




maaaah@debian:~/architect-bionicpro$ docker exec -it crm-oracle /bin/bash
bash-4.4$ sqlplus CRM/crm@//localhost:1521/XEPDB1

SQL*Plus: Release 21.0.0.0.0 - Production on Sun Nov 22 10:50:03 2026
Version 21.3.0.0.0

Copyright (c) 1982, 2021, Oracle.  All rights reserved.

Last Successful login time: Sun Nov 22 2026 10:49:19 +00:00

Connected to:
Oracle Database 21c Express Edition Release 21.0.0.0.0 - Production
Version 21.3.0.0.0

SQL> select FIRST_NAME, LAST_NAME from PATIENTS;

FIRST_NAME
--------------------------------------------------------------------------------
LAST_NAME
--------------------------------------------------------------------------------
John
Doe

Alex
Johnson

Prothetic
One


FIRST_NAME
--------------------------------------------------------------------------------
LAST_NAME
--------------------------------------------------------------------------------
Prothetic
Two

Prothetic
Three


docker logs airflow-standalone | fgrep 'Login with username'
