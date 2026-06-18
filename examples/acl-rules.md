# Writing lakeFS ACL rules

How to author access-control policies for lakeFS when running with the reference
**ACL server** (`auth.api.endpoint`). This is what lets you go beyond the four built-in
groups and do **per-repo** / fine-grained access.

> Why the ACL server: in `rbac=simplified` mode lakeFS's own API blocks custom policy
> creation (`501 Not Implemented`). The underlying engine is full RBAC, so you create
> custom policies by talking to the **ACL server** directly (`http://<aclserver>:8001/api/v1/auth`,
> reachable on the internal network, bearer-less). Policies attach to **groups**; users get
> permissions via group membership.

---

## 1. The policy model

A policy is a list of **statements**. Each statement = `effect` + `action[]` + `resource`:

```json
{
  "name": "demo-readonly",
  "statement": [
    { "effect": "allow", "action": ["fs:ReadObject", "fs:ListObjects"], "resource": "arn:lakefs:fs:::repository/demo/*" }
  ]
}
```

- **effect**: `allow` or `deny`. (`deny` always wins over `allow`.)
- **action**: one or more action strings (see catalog below). Wildcards work: `fs:*`, `fs:Read*`, `retention:*`.
- **resource**: an ARN the action applies to, or `*` for everything. Wildcards allowed in the resource (`.../repository/demo/*`).
- A request is **allowed only if** some statement matches its action + resource with `allow`, and no `deny` matches.

Variable: **`${user}`** expands to the calling user's id — use it for "manage your own X"
(e.g. `arn:lakefs:auth:::user/${user}`).

---

## 2. Resource ARN formats

| Resource | ARN |
|----------|-----|
| Everything | `*` |
| A repository | `arn:lakefs:fs:::repository/REPO` |
| Objects in a repo | `arn:lakefs:fs:::repository/REPO/object/KEY` (use `.../object/*` or `.../REPO/*`) |
| A branch | `arn:lakefs:fs:::repository/REPO/branch/BRANCH` |
| A tag | `arn:lakefs:fs:::repository/REPO/tag/TAG` |
| A storage namespace | `arn:lakefs:fs:::namespace/NAMESPACE` |
| A user | `arn:lakefs:auth:::user/USER` |
| A group | `arn:lakefs:auth:::group/GROUP` |
| A policy | `arn:lakefs:auth:::policy/POLICY` |
| An external principal | `arn:lakefs:auth:::externalPrincipal/ID` |
| Catalog namespace / table / view | `arn:lakefs:catalog:::namespace/REPO/NS` · `.../table/REPO/NS/TABLE` · `.../view/REPO/NS/VIEW` |

To scope a repo's *contents*, the common pattern is two resources: the repo itself
(`.../repository/REPO`) **and** its sub-objects (`.../repository/REPO/*`).

---

## 3. Action catalog (every ACL item)

**Scope** column tells you what `resource` the action is checked against: `*` = global
(must be `*`), `repo` = a repository ARN (or its `/object`,`/branch`,`/tag` children),
`namespace`/`user`/`group`/`policy`/`catalog` = those ARN types.

### `fs:` — data plane (repos, objects, branches, commits, tags)
| Action | Scope | Meaning |
|--------|-------|---------|
| `fs:ListRepositories` | `*` | list repos (**global** — can't scope per repo; see gotcha) |
| `fs:CreateRepository` | `*` | create a repo |
| `fs:ReadConfig` | `*` | read server config (UI needs this) |
| `fs:ReadRepository` | repo | view a repo |
| `fs:UpdateRepository` | repo | change repo settings |
| `fs:DeleteRepository` | repo | delete a repo |
| `fs:AttachStorageNamespace` | namespace | use a storage namespace when creating a repo |
| `fs:ImportFromStorage` | namespace | import existing objects from the bucket |
| `fs:ImportCancel` | repo | cancel an import |
| `fs:ReadObject` | repo/object | read object data |
| `fs:WriteObject` | repo/object | upload/overwrite object |
| `fs:DeleteObject` | repo/object | delete object |
| `fs:ListObjects` | repo | list objects |
| `fs:ReadBranch` | repo/branch | read a branch ref |
| `fs:ListBranches` | repo | list branches |
| `fs:CreateBranch` | repo/branch | create a branch |
| `fs:DeleteBranch` | repo/branch | delete a branch |
| `fs:RevertBranch` | repo/branch | revert commits on a branch |
| `fs:CreateCommit` | repo/branch | commit |
| `fs:ReadCommit` | repo | read a commit |
| `fs:ListCommits` | repo | list commits |
| `fs:CreateTag` | repo/tag | create a tag |
| `fs:DeleteTag` | repo/tag | delete a tag |
| `fs:ReadTag` | repo/tag | read a tag |
| `fs:ListTags` | repo | list tags |

### `auth:` — users, groups, policies, credentials (admin plane)
| Action | Scope |
|--------|-------|
| `auth:CreateUser` `auth:DeleteUser` `auth:ReadUser` `auth:ListUsers` | user / `*` |
| `auth:CreateGroup` `auth:DeleteGroup` `auth:ReadGroup` `auth:ListGroups` | group / `*` |
| `auth:AddGroupMember` `auth:RemoveGroupMember` | group |
| `auth:CreatePolicy` `auth:DeletePolicy` `auth:UpdatePolicy` `auth:ReadPolicy` `auth:ListPolicies` | policy / `*` |
| `auth:AttachPolicy` `auth:DetachPolicy` | group/user |
| `auth:CreateCredentials` `auth:DeleteCredentials` `auth:ReadCredentials` `auth:ListCredentials` | user (use `${user}` for self) |
| `auth:CreateUserExternalPrincipal` `auth:DeleteUserExternalPrincipal` `auth:ReadExternalPrincipal` | user / externalPrincipal |

### `retention:` — garbage collection
| Action | Scope |
|--------|-------|
| `retention:GetGarbageCollectionRules` `retention:SetGarbageCollectionRules` | repo |
| `retention:PrepareGarbageCollectionCommits` `retention:PrepareGarbageCollectionUncommitted` | repo |

### `branches:` — branch protection rules
| Action | Scope |
|--------|-------|
| `branches:GetBranchProtectionRules` `branches:SetBranchProtectionRules` | repo |

### `pr:` — pull requests
| Action | Scope |
|--------|-------|
| `pr:ListPullRequests` `pr:ReadPullRequest` `pr:WritePullRequest` | repo |

### `ci:` — actions/hooks
| Action | Scope |
|--------|-------|
| `ci:ReadAction` | repo |

### `catalog:` — table/namespace catalog
| Action | Scope |
|--------|-------|
| `catalog:CreateNamespace` `catalog:GetNamespace` `catalog:ListNamespaces` `catalog:UpdateNamespace` `catalog:DeleteNamespace` | catalog namespace |
| `catalog:CreateTable` `catalog:ReadTable` `catalog:ListTables` `catalog:UpdateTable` `catalog:DeleteTable` | catalog table |
| `catalog:CreateView` `catalog:ReadView` `catalog:ListViews` `catalog:UpdateView` `catalog:DeleteView` | catalog view |

---

## 4. The four built-in groups (for reference)

`SetupACLServer` creates these; each is **all-repos** (you can't scope them — that's what
custom policies are for):

| Group | ≈ grants |
|-------|----------|
| `Readers` | `fs:List*`, `fs:Read*` on `*` + manage own credentials |
| `Writers` | read + write data (`fs:*Object`, branches, commits…) on `*` + repo-management read |
| `Supers` | full `fs:*` on `*` (incl. create/delete repos) + manage own credentials |
| `Admins` | everything, incl. all `auth:*` (manage users/groups/policies) |

---

## 5. Recipes

### Read-only on one repo (`demo`)
```json
{
  "name": "demo-ro",
  "statement": [
    { "effect": "allow", "action": ["fs:ListRepositories","fs:ReadConfig"], "resource": "*" },
    { "effect": "allow", "action": ["fs:ReadRepository","fs:ReadObject","fs:ListObjects","fs:ReadBranch","fs:ListBranches","fs:ReadCommit","fs:ListCommits","fs:ReadTag","fs:ListTags"], "resource": "arn:lakefs:fs:::repository/demo" },
    { "effect": "allow", "action": ["fs:ReadObject","fs:ListObjects"], "resource": "arn:lakefs:fs:::repository/demo/*" },
    { "effect": "allow", "action": ["auth:CreateCredentials","auth:ListCredentials","auth:DeleteCredentials"], "resource": "arn:lakefs:auth:::user/${user}" }
  ]
}
```

### Read-write on one repo
Add the write actions to the repo statements:
```json
{ "effect": "allow",
  "action": ["fs:WriteObject","fs:DeleteObject","fs:CreateBranch","fs:DeleteBranch","fs:CreateCommit","fs:RevertBranch","fs:CreateTag","fs:DeleteTag"],
  "resource": "arn:lakefs:fs:::repository/demo" },
{ "effect": "allow", "action": ["fs:WriteObject","fs:DeleteObject"], "resource": "arn:lakefs:fs:::repository/demo/*" }
```

### Full control of one repo (incl. GC, branch protection, PRs) — repo "admin"
```json
{
  "name": "demo-full",
  "statement": [
    { "effect": "allow", "action": ["fs:ListRepositories","fs:ReadConfig"], "resource": "*" },
    { "effect": "allow", "action": ["fs:*","retention:*","branches:*","ci:*","pr:*"], "resource": "arn:lakefs:fs:::repository/demo" },
    { "effect": "allow", "action": ["fs:*"], "resource": "arn:lakefs:fs:::repository/demo/*" },
    { "effect": "allow", "action": ["auth:CreateCredentials","auth:ListCredentials","auth:DeleteCredentials"], "resource": "arn:lakefs:auth:::user/${user}" }
  ]
}
```

### Read-write on ALL repos (just use the built-in `Writers` group — no custom policy needed)

---

## 6. Applying a policy

Create the policy, a group, attach, add users — all on the ACL server:

```bash
ACL=http://<aclserver>:8001/api/v1/auth     # internal network

# 1. create the policy
curl -s -X POST "$ACL/policies" -H 'Content-Type: application/json' -d @demo-ro.json

# 2. create a group and attach the policy
curl -s -X POST "$ACL/groups" -H 'Content-Type: application/json' -d '{"id":"DemoReaders"}'
curl -s -X PUT  "$ACL/groups/DemoReaders/policies/demo-ro"

# 3. put users in the group (or map an IdP group of the same name via the SSO shim)
curl -s -X PUT  "$ACL/groups/DemoReaders/members/alice"
```

With the SSO shim, just create an **IdP group named `DemoReaders`**; members are mapped in on login.

---

## 7. Gotchas

- **`fs:ListRepositories` is global** (`*` only). A scoped user can still *see other repos'
  names* in listings, but not their contents. There's no per-repo way to hide names.
- **Two resources per repo**: grant on both `.../repository/REPO` (repo-level ops) and
  `.../repository/REPO/*` (object-level ops), or you'll get "can list but not read" oddities.
- **`deny` beats `allow`** — use a `deny` statement to carve out exceptions.
- **No GUI**: these custom policies aren't shown/editable in the lakeFS UI under
  `rbac=simplified`; manage them via the ACL server API.
- **Include `fs:ReadConfig` on `*`** or the web UI won't load for that user.
