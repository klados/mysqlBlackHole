# mysqlBlackHole

A fake/simulated MySQL server ("black hole" — data goes in, nothing persists) written in Go.

It speaks the real MySQL wire protocol via the pinned library
[`github.com/go-mysql-org/go-mysql`](https://github.com/go-mysql-org/go-mysql), but all
read data lives in **Redis** instead of a real database. Every write path is denied.

## How it works

- `main.go` opens a TCP listener on `PORT` (default `3306`); per connection it
  authenticates with `BlackHoleAuthHandler` (credentials in a Redis hash `mysql_users`),
  then loops on `conn.HandleCommand()`.
- The `go-mysql` library dispatches protocol `COM_*` packets and calls into
  `BlackHoleHandler` (implements `server.Handler`).
- `BlackHoleHandler.HandleQuery` switches on normalized SQL text and delegates to the
  helpers in `query_handlers.go`, `select_handler.go`, and `describe_handler.go`.
- **Read path (real):** `SELECT` is parsed with the TiDB parser; table data is loaded from
  Redis JSON keys (`table_data:<db>:<table>`) and filtered/sorted/limited/projected by
  the expression evaluator in `expr_eval.go`.
- **Write path (black hole):** `INSERT`, `UPDATE`, `DELETE`, `CREATE`, `DROP`, `TRUNCATE`,
  `ALTER`, `RENAME`, `GRANT`, `REVOKE` are parsed (to extract the target database) and
  always denied with a MySQL-style `Access denied` error.

A companion seeder (`cmd/seed-redis`) loads `seed_data.yaml` into Redis (a fake M&A deal
database plus listings of `information_schema`/`mysql`/`performance_schema` tables).

## Configuration

Copy `.env.example` to `.env`:

| Variable    | Default   | Description            |
|-------------|-----------|------------------------|
| `PORT`      | `3306`    | TCP port to listen on  |
| `REDIS_ADDR`| (see .env)| Redis connection addr   |

> Inside Docker Compose the app reaches Redis by the service name (`redis:6379`);
> `.env` / host runs use `localhost:6379`.

## Running

```sh
# full stack: Redis, black-hole server, Elasticsearch, Kibana, Vector
docker compose up -d --build

# seed fake data into Redis (one-shot; its logs also flow to Elasticsearch)
docker compose run --rm seed

# optional: run the server on the host instead of in Docker
go run .
```

To try it out, connect a MySQL client to `localhost:3306` with a seeded user.

### Logs & observability

Every log line is JSON written to stdout and tagged with `"service":"mysql-blackhole"`
(server) or `"service":"seed-redis"` (seeder). Vector (`vector.yaml`) watches the
labelled containers and ships them to Elasticsearch index `mysqlblackhole`.

- Kibana: <http://localhost:5601>
- Elasticsearch: <http://localhost:9200>
- Vector API/health: <http://localhost:8686/health>

## Deploying to a server

Only the honeypot port (`3306`) is meant to be public. The observability stack
(Elasticsearch, Kibana, Vector API) and Redis are bound to `localhost` only and are
reached through an SSH tunnel, so attackers probing the server cannot touch your logs or
the seeded data.

1. Copy the project to the server and start the stack:

   ```sh
   docker compose up -d --build
   docker compose run --rm seed
   ```

   Only `3306/tcp` is published; `9200`, `5601`, `8686`, and `6379` are not reachable
   from the network.

2. From your admin machine, tunnel in to view Kibana:

   ```sh
   ssh -L 5601:localhost:5601 user@server
   ```

   then open <http://localhost:5601> locally.

3. Lock down the machine with a firewall (UFW):

   ```sh
   ufw default deny incoming
   ufw default allow outgoing
   ufw allow 22/tcp      # SSH (your tunnel)
   ufw allow 3306/tcp    # the honeypot
   ufw enable
   ```

   On a cloud VPS, apply the equivalent in the security group: inbound only `22` and
   `3306`.


## Supported command surface

| Command | Behavior | Location |
|---------|----------|----------|
| `SHOW DATABASES` / `SHOW SCHEMAS` | Lists DBs the user may access | `query_handlers.go` `handleShowDatabases` |
| `SHOW TABLES [FROM/IN db]` | Lists seeded tables for a DB | `query_handlers.go` `handleShowTables` |
| `SELECT DATABASE()` | Current DB or `NULL` | `query_handlers.go` `handleSelectDatabase` |
| `SELECT VERSION()` | Fixed version string | `query_handlers.go` `handleSelectVersion` |
| `USE <db>` | SELECTs current DB (COM_QUERY) | `query_handlers.go` `handleUse` |
| `DESCRIBE` / `DESC <table>` | One row per seeded column | `describe_handler.go` |
| `SELECT ...` (single table) | Real WHERE/ORDER BY/LIMIT/projections | `select_handler.go` + `expr_eval.go` |
| `CREATE/DROP DATABASE`, `CREATE/DROP TABLE`, `TRUNCATE`, `DELETE`, `INSERT`/`REPLACE`, `UPDATE`, `ALTER`, `RENAME`, `GRANT`/`REVOKE` | Denied with `Access denied` | `query_handlers.go` |
| `SHOW CREATE TABLE`, `SHOW TABLE STATUS`, `SHOW FIELDS`, `SHOW COLUMNS`, `SHOW KEYS`, `SHOW INDEX`, `SHOW VARIABLES`, `information_schema`, `@@COLLATION_DATABASE` | Denied with `ER_TABLEACCESS_DENIED_ERROR` (1142) so dump tools fail cleanly | `handler.go` `HandleQuery` |

Anything else (e.g. `SET ...`, unknown) is silently accepted and returns an empty success
result (`handler.go` default case).

## Future Development

Penetration-testing tools and scanners send a number of MySQL commands/queries that the
server currently does not handle realistically. They either fall through to a blank
empty-success (from the `HandleQuery` default case) or return `"not supported"` (from
`HandleOtherCommand`). These are candidates for future work to improve honeypot deception.

### A. Unhandled `COM_*` protocol commands

Routed to `HandleOtherCommand` (`handler.go:126`) which always returns `"not supported"`.
Used by `mysqladmin` and network scanners; should return realistic responses.

- `COM_PROCESS_INFO` (0x0a) — `mysqladmin processlist`
- `COM_STATISTICS` (0x09) — `mysqladmin status`
- `COM_REFRESH` (0x07), `COM_SHUTDOWN` (0x08)
- `COM_SET_OPTION` (0x1b) — sent by many clients
- Replication commands: `COM_BINLOG_DUMP` (0x12), `COM_REGISTER_SLAVE` (0x15),
  `COM_BINLOG_DUMP_GTID` (0x1e) — no `ReplicationHandler` is wired in `main.go`

### B. `@@` system-variable queries

Currently fall to silent empty-success. Used by sqlmap/msf/nmap for banner-grabbing and
fingerprinting.

- `@@hostname`, `@@port`, `@@version`, `@@version_comment`
- `@@datadir`, `@@basedir`, `@@secure_file_priv`
- `@@character_set_*`, `@@wait_timeout`, `@@max_allowed_packet`

### C. User / privilege introspection

Currently silent empty-success; should return realistic privilege errors (mirroring the
existing 1142 pattern at `handler.go:69`) so enumeration tools fail cleanly.

- `SELECT user,host FROM mysql.user` — nmap `mysql-users` script / msf `mysql_enum`
- `SHOW GRANTS`, `SHOW GRANTS FOR CURRENT_USER()`
- `SELECT USER()`, `SELECT CURRENT_USER()`, `SELECT SESSION_USER()`

### D. `SHOW STATUS` / `SHOW PROCESSLIST`

Fall to silent empty-success. `SHOW STATUS`, `SHOW GLOBAL STATUS`, and
`SHOW PROCESSLIST` are commonly probed by scanners to enumerate runtime state.

### E. File-read / SQLi probes

Silently accepted; these are common injection probes and ideally should not return a
blank success.

- `LOAD_FILE('<path>')`
- `SELECT ... INTO OUTFILE`, `SELECT ... INTO DUMPFILE`

### F. Prepared statements

`COM_STMT_PREPARE`, `COM_STMT_EXECUTE`, `COM_STMT_CLOSE` are no-ops / return empty
results (`handler.go:108-122`). Some tooling (e.g. sqlmap) uses prepared statements and
would behave more believably with real handling.
