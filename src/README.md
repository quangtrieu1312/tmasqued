# tmasqued server internals (`src/`)

For day-to-day administration you do **not** need this — use the `tmasquectl`
wrapper (see the [top-level README](../README.md)). This document is for people
who want to **contribute** to the server or **troubleshoot the database** directly.

## Layout

```
main.go            QUIC listener, per-client tunChan map, forward consumer (SNAT + TX)
management.go      the management HTTP API (Unix socket) — routing + validation
xdp/               AF_XDP conn, eBPF loader, RX dispatch (QUIC vs NAT-return)
utility/           ForwardBatch (AF_XDP/kernel TX split), SNAT, reseq, GSO helpers
config/            reads tmasqued.conf into context; inotify hot-reload (LOG_LEVEL, ENABLE_STATISTIC)
stats/             the independent on/off STATISTIC channel (separate from the leveled logger)
logger/            leveled logger (FATAL..TRACE); ShouldLog() guards before building a message
db/                sqlite connection singleton
domain/            row structs (Client, Role, Resource, DHCP)
request/ response/ management API request bodies / response bodies
repository/        all SQL, one transaction per call
service/           thin, context-aware pass-throughs over repository
migration/         schema migrations (run once at boot)
```

## Control-plane architecture

```
management.go  ──►  service/  ──►  repository/  ──►  db/ (sqlite)
 (HTTP, JSON)      (thin wrap)     (SQL + tx)
```

`management.go` parses the request and calls a `service` function; `service`
just forwards to `repository`, which owns the SQL and transactions. `domain`
holds the row structs; `request`/`response` are the JSON DTOs.

## Data model

| Table | Columns | Notes |
|---|---|---|
| `clients` | id, **name UNIQUE**, ip, last_seen | a VPN client (`/32`) |
| `roles` | id, **name UNIQUE** | a group of resources |
| `resources` | id, **name UNIQUE**, value | a named CIDR/route |
| `clients_roles` | client_id, role_id | M:N, UNIQUE(client_id, role_id) |
| `roles_resources` | role_id, resource_id | M:N, UNIQUE(role_id, resource_id) |
| `dhcp` | first_ip, last_ip | free IP ranges (merged on release) |

A client's **effective routes** = the union of the resources granted to all the
roles linked to that client (`GET /clients/{id}/resources`).

## Management API (`/var/run/tmasqued.sock`)

Every create / link / unlink / delete responds with `{"ids":[…]}` — the ids
**actually affected** (link ops report only the rows that changed).

**Clients**
| Method + path | Body | Purpose |
|---|---|---|
| `GET /clients` | — | list |
| `POST /clients` | `{"names":[…]}` | create (+ default role); **fails on duplicate name** |
| `GET /clients/{id}` | — | one client |
| `PATCH /clients/{id}` | `{"name":"…"}` | rename (also renames the default role) |
| `DELETE /clients/{id}` | — | delete (also deletes the default role) |
| `GET /clients/{id}/roles` | — | the client's roles |
| `GET /clients/{id}/resources` | — | the client's effective routes |
| `POST` / `DELETE /clients/{id}/roles` | `{"role_ids":[…]}` | link / unlink roles by id |
| `POST` / `DELETE /clients/by-name/{name}/roles` | `{"role_names":[…]}` | link / unlink roles by name |

**Roles**
| Method + path | Body | Purpose |
|---|---|---|
| `GET /roles` · `GET /roles/{id}` | — | list / one |
| `POST /roles` | `{"names":[…]}` | create; **fails on duplicate name** |
| `PATCH /roles/{id}` | `{"name":"…"}` | rename |
| `DELETE /roles/{id}` | — | delete |
| `POST` / `DELETE /roles/{id}/resources` | `{"resource_ids":[…]}` | grant / revoke by id |
| `POST` / `DELETE /roles/by-name/{name}/resources` | `{"resource_names":[…]}` | grant / revoke by name |

**Resources**
| Method + path | Body | Purpose |
|---|---|---|
| `GET /resources` · `GET /resources/{id}` | — | list / one |
| `POST /resources` | `{"resources":[{"name":"…","value":"CIDR"}]}` | create; **fails on duplicate name** |
| `PATCH /resources/{id}` | `{"name":"…"}` | rename |
| `DELETE /resources/{id}` | — | delete |

**DHCP** — `GET /dhcp` (free ranges) · `PUT /dhcp` `{"first_ip":N,"last_ip":N}` (reset pool).

## Behaviors & gotchas (read before debugging the DB)

- **Names are unique and create rejects duplicates.** `POST` does a plain INSERT;
  a repeat name hits the `UNIQUE(name)` constraint → the API returns 400. (There is
  no upsert; to change a resource's value, delete and recreate it.)
- **Foreign-key cascades are ON** (`_foreign_keys=on` in the DSN — `db/db.go`). The
  junction tables declare `ON DELETE CASCADE`, so deleting a client/role/resource
  removes its **link rows** but **not** the entities on the other side — deleting a
  client keeps its roles; deleting a role keeps its clients and resources. So "the data
  still exists after you remove something it was linked to", with no dangling junction
  rows. Assigning a link to a non-existent id now fails (FK violation) instead of
  silently inserting a dangling row. (FK enforcement is off by default in SQLite and
  must be set per-connection — earlier builds left it off, which let orphaned junction
  rows accumulate and, via SQLite id-reuse, get inherited by new entities; `_foreign_keys=on`
  closes that. Check any legacy DB with `PRAGMA foreign_key_check;`.)
- **Concurrency.** The pool is capped at one connection (`SetMaxOpenConns(1)`) with
  `_busy_timeout=5000` and WAL, so concurrent management/connect requests serialize
  in-process rather than racing SQLite's single-writer file lock (no "database is
  locked" errors). The control plane is low-traffic, so this is fine.
- **Config & hot-reload.** `config.Load` parses `KEY=value` (full-line `#` comments and
  blank lines skipped; split on the first `=`, key/value trimmed). `config.Watch` then
  watches the file via inotify and hot-applies `LOG_LEVEL` and `ENABLE_STATISTIC` on
  change; every other key is boot-only and needs a restart. Statistics are their own
  on/off channel (the `stats` package), **not** a logger level — the `[STATISTIC]` lines,
  the per-packet observers, and the pprof/expvar endpoint are all gated by it.
- **The "default role".** Creating a client auto-creates and links a role with the
  **same name** (so a fresh client always has its own role). Two explicit rules keep
  them in sync (cascades can't, since FK is off): renaming a client renames that
  role, and deleting a client deletes it. Both match on the client's *current* name.
- **`Upsert*` is a legacy name** — those repository/service functions now create-or-
  fail, not upsert.

## Inspecting the database

The DB lives in the `db` Docker volume at `/etc/tmasqued/data/tmasqued.db`:

```sh
sudo docker compose exec tmasqued sqlite3 /etc/tmasqued/data/tmasqued.db \
  "SELECT c.name AS client, r.name AS role, res.name AS resource, res.value
   FROM clients c
   LEFT JOIN clients_roles cr ON cr.client_id = c.id
   LEFT JOIN roles r ON r.id = cr.role_id
   LEFT JOIN roles_resources rr ON rr.role_id = r.id
   LEFT JOIN resources res ON res.id = rr.resource_id;"
```

Migrations live in `migration/` and run once at boot (tracked in the `migrations`
table); add a new `N_*.go` rather than editing `1_create_tables.go`.
