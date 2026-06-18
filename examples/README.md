# Example: lakeFS + ACL server + SSO shim

Self-contained stack that gives open-source lakeFS multi-user groups **and** OIDC SSO.

## Bring it up

```bash
cp .env.example .env
# edit .env: set SHARED_SECRET (openssl rand -hex 24) and your OIDC_* values
docker compose up -d --build
```

- `aclserver` is compiled from the lakeFS `contrib/auth/acl` reference server (first build clones lakeFS, takes a few minutes).
- On startup it auto-creates the default groups: **Admins, Supers, Writers, Readers**.

## Bootstrap an admin (one-time)

OSS lakeFS with an external ACL server starts with no users. Create an admin directly on the ACL server:

```bash
ACL="http://localhost:8001/api/v1/auth"   # or `docker compose exec` if 8001 isn't published
docker compose exec aclserver sh -c '
  apk add --no-cache curl >/dev/null 2>&1 || true
  curl -s -X POST http://localhost:8001/api/v1/auth/users    -H "Content-Type: application/json" -d "{\"username\":\"admin\"}"
  curl -s -X PUT  http://localhost:8001/api/v1/auth/groups/Admins/members/admin
  curl -s -X POST http://localhost:8001/api/v1/auth/users/admin/credentials
'
```

The last call returns an `access_key_id` / `secret_access_key` — use them for `lakectl`, the S3 gateway, or direct UI login.

## Per-repo groups (optional)

Full reference (all actions, ARN formats, recipes): [acl-rules.md](acl-rules.md).

The 4 default groups apply to **all** repos. To scope a group to one repo, create a custom policy on the ACL server and attach it:

```bash
# read-only on repo "myrepo"
curl -s -X POST http://localhost:8001/api/v1/auth/policies -H "Content-Type: application/json" -d '{
  "name":"myrepo-ro",
  "statement":[
    {"effect":"allow","action":["fs:ListRepositories","fs:ReadConfig"],"resource":"*"},
    {"effect":"allow","action":["fs:ReadRepository","fs:ReadObject","fs:ListObjects","fs:ReadBranch","fs:ListBranches","fs:ReadCommit"],"resource":"arn:lakefs:fs:::repository/myrepo"},
    {"effect":"allow","action":["fs:ReadObject","fs:ListObjects"],"resource":"arn:lakefs:fs:::repository/myrepo/*"},
    {"effect":"allow","action":["auth:CreateCredentials","auth:ListCredentials","auth:DeleteCredentials"],"resource":"arn:lakefs:auth:::user/${user}"}
  ]
}'
curl -s -X POST http://localhost:8001/api/v1/auth/groups -H "Content-Type: application/json" -d '{"id":"MyRepoReaders"}'
curl -s -X PUT  http://localhost:8001/api/v1/auth/groups/MyRepoReaders/policies/myrepo-ro
```

Then create an IdP group named `MyRepoReaders` (same name) and the shim maps members into it on login.

## Log in

Send users to **http://localhost:8088/_shim/login** → they authenticate at your IdP and land in lakeFS, mapped into the lakeFS group(s) matching their IdP groups (or `LAKEFS_DEFAULT_GROUP`).

API/CLI clients keep using lakeFS access keys directly (the shim proxies them through, or expose lakeFS :8000).
