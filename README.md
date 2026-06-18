# lakefs-sso-shim

A tiny reverse-proxy that adds **OIDC single sign-on** to **open-source lakeFS**, which otherwise has no SSO (it's an [lakeFS Enterprise / Fluffy](https://docs.lakefs.io/enterprise/) feature).

The shim sits in front of lakeFS, runs the OIDC authorization-code flow against your IdP (Authentik, Keycloak, Okta, …), provisions the user into lakeFS groups based on the token's `groups` claim, mints a lakeFS login token, and drops it into the `internal_auth_session` cookie that lakeFS already trusts. Everything else is transparently proxied to lakeFS.

## How it works

```
browser ──/_shim/login──▶ shim ──OIDC redirect──▶ IdP
        ◀──────────────── shim ◀──/oidc/callback── IdP (code)
                            │  exchange code, read username + groups
                            │  provision user + sync groups via lakeFS auth API
                            │  mint HS256 lakeFS login JWT (shared secret)
                            │  Set-Cookie: internal_auth_session=<gorilla securecookie>
        ◀──302 to / ────────┘
browser ──any request────▶ shim ──reverse proxy──▶ lakeFS  (cookie auth)
```

The login token is a standard lakeFS `LoginClaims` JWT (`iss=auth`, `sub=<username>`, `aud=login`), HS256-signed with lakeFS's `auth.encrypt.secret_key`. The cookie is encoded with `gorilla/sessions` exactly as lakeFS expects.

Requires a lakeFS auth backend that supports user/group management via the auth API — i.e. lakeFS configured with an external **ACL server** (`auth.api.endpoint`). API/CLI clients using access keys keep working directly against lakeFS, untouched.

## Configuration (env vars)

| Var | Required | Default | Description |
|-----|----------|---------|-------------|
| `LAKEFS_SHARED_SECRET` | ✅ | | Must equal lakeFS `auth.encrypt.secret_key` |
| `OIDC_ISSUER` | ✅ | | OIDC issuer URL (exact, incl. trailing slash if the IdP uses one) |
| `OIDC_CLIENT_ID` | ✅ | | OAuth2 client id |
| `OIDC_CLIENT_SECRET` | | | OAuth2 client secret (confidential client) |
| `OIDC_REDIRECT_URL` | ✅ | | `https://lakefs.example.com/oidc/callback` |
| `LAKEFS_UPSTREAM` | | `http://lakefs:8000` | lakeFS base URL to proxy to |
| `ACL_BASE` | | `http://lakefs-aclserver:8001/api/v1/auth` | lakeFS auth (ACL server) API base |
| `LISTEN_ADDR` | | `:8088` | shim listen address |
| `OIDC_GROUPS_CLAIM` | | `groups` | token claim holding group names |
| `OIDC_USERNAME_CLAIM` | | `preferred_username` | token claim for the lakeFS username (falls back to `sub`) |
| `LAKEFS_DEFAULT_GROUP` | | `Readers` | group assigned when no token group matches a lakeFS group |

Group mapping is **by name**: an IdP group is mapped to the lakeFS group with the same name, if it exists.

## Quick start

A complete, self-contained stack (lakeFS + reference ACL server + this shim) is in
[`examples/docker-compose.yml`](examples/docker-compose.yml). It's the fastest way to
see the whole thing working:

```bash
cd examples
cp .env.example .env        # set SHARED_SECRET + your OIDC_* values
docker compose up -d --build
```

See [`examples/README.md`](examples/README.md) for the one-time admin bootstrap and
how to create per-repo groups.

## Run (shim only)

If you already run lakeFS + an ACL server, run just the shim:

```bash
docker run -d --name lakefs-sso-shim -p 8088:8088 \
  -e LAKEFS_SHARED_SECRET=... \
  -e OIDC_ISSUER=https://sso.example.com/application/o/lakefs/ \
  -e OIDC_CLIENT_ID=... -e OIDC_CLIENT_SECRET=... \
  -e OIDC_REDIRECT_URL=https://lakefs.example.com/oidc/callback \
  -e LAKEFS_UPSTREAM=http://lakefs:8000 \
  -e ACL_BASE=http://lakefs-aclserver:8001/api/v1/auth \
  ghcr.io/leoleoasd/lakefs-sso-shim:latest
```

Point users at the shim (`:8088`); send them to `/_shim/login` to start SSO. `/_shim/logout` clears the session.

## Caveats

- **UI login only.** OIDC covers the browser; programmatic/CLI access still uses lakeFS access keys.
- **Group sync is additive** in this version — it adds memberships but does not remove stale ones. Removing a user from an IdP group won't revoke the matching lakeFS group until that's reconciled manually.
- `fs:ListRepositories` is global in lakeFS, so scoped users can still *see* other repo names (not their contents).
- The shim holds the lakeFS shared secret (it can mint a token for any user). Treat it as a trusted component.

## SCIM provisioning / deprovisioning (optional)

Set `SCIM_TOKEN` to enable a SCIM 2.0 endpoint at `/scim/v2`. Point your IdP's SCIM
provider at `https://<shim-host>/scim/v2` with `Authorization: Bearer <SCIM_TOKEN>`.

The IdP then pushes user/group lifecycle in real time onto the lakeFS ACL server:

| IdP action | effect in lakeFS |
|------------|------------------|
| create user / add to group | user created, added to same-named lakeFS group |
| remove from group | membership removed |
| **delete or deactivate user** | **lakeFS user deleted → all access & keys revoked** |

This is the proper fix for deprovisioning: it's push-based and real-time, so it also
covers users who never log in again (unlike login-time group sync). User id == `userName`,
group id == `displayName`.

| Var | Description |
|-----|-------------|
| `SCIM_TOKEN` | bearer token the IdP must present; enables `/scim/v2` when set |

## License

MIT
