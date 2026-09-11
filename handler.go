package main

import (
	"context"
	"fmt"
	"log/slog"
	"mysqlBlackHole/model/sqlemulate"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/redis/go-redis/v9"
)

type BlackHoleHandler struct {
	currentDB   string
	username    string
	redis       *redis.Client
	fingerprint ClientFingerprint
	connID      uint32
}

// SetUsername stores the authenticated username so DB access can be scoped per user.
func (h *BlackHoleHandler) SetUsername(username string) {
	h.username = username
}

// SetFingerprint stores the client fingerprint so log lines can be attributed
// to a specific client.
func (h *BlackHoleHandler) SetFingerprint(fp ClientFingerprint) {
	h.fingerprint = fp
}

// SetConnID stores the server-assigned connection ID for log correlation.
func (h *BlackHoleHandler) SetConnID(id uint32) {
	h.connID = id
}

func (h *BlackHoleHandler) logAttrs(extra ...any) []any {
	attrs := []any{slog.Int("conn_id", int(h.connID))}
	attrs = append(attrs, h.fingerprint.AttrsSlice()...)
	return append(attrs, extra...)
}

// UseDB handles the COM_INIT_DB packet, sent when a client executes "USE <db>".
// It validates the authenticated user is allowed to use the database and tracks
// it as the current DB.
func (h *BlackHoleHandler) UseDB(dbName string) error {
	ctx := context.Background()
	allowed, err := h.redis.SIsMember(ctx, sqlemulate.GetSupportedDBUsersKey(dbName), h.username).Result()
	if err != nil {
		slog.Error("use_db redis error", h.logAttrs(slog.String("db", dbName), slog.Any("err", err))...)
		return err
	}
	if !allowed {
		slog.Warn("use_db denied", h.logAttrs(slog.String("db", dbName))...)
		return fmt.Errorf("Access denied for user '%s' to database '%s'", h.username, dbName)
	}

	h.currentDB = dbName
	slog.Info("use_db", h.logAttrs(slog.String("db", dbName))...)
	return nil
}

// HandleQuery handles the COM_QUERY packet, covering ordinary SQL statements such as
// SELECT, SHOW, and others. It inspects the query and delegates to the matching
// helper in query_handlers.go.
func (h *BlackHoleHandler) HandleQuery(query string) (*mysql.Result, error) {
	upper := strings.ToUpper(strings.Join(strings.Fields(strings.TrimSpace(query)), " "))

	slog.Info("query", h.logAttrs(slog.String("sql", query), slog.String("db", h.currentDB))...)

	switch {
	case upper == "SHOW DATABASES" || upper == "SHOW SCHEMAS":
		return handleShowDatabases(h.redis, h.username)
	case strings.HasPrefix(upper, "SHOW TABLES"):
		return handleShowTables(h.redis, h.currentDB, h.username, query)
	case upper == "SELECT DATABASE()" || upper == "SELECT DATABASE() AS `DATABASE()`":
		return handleSelectDatabase(h.currentDB)
	case strings.HasPrefix(upper, "SELECT") && strings.Contains(upper, "VERSION"):
		return handleSelectVersion()
	case strings.HasPrefix(upper, "USE "): // used when receive package COM_QUERY
		return handleUse(h, query)
	case strings.HasPrefix(upper, "DESCRIBE ") || strings.HasPrefix(upper, "DESC "):
		return handleDescribe(h.redis, h.currentDB, query)
	case strings.HasPrefix(upper, "SHOW CREATE TABLE") ||
		strings.HasPrefix(upper, "SHOW TABLE STATUS") ||
		strings.HasPrefix(upper, "SHOW FIELDS") || strings.HasPrefix(upper, "SHOW COLUMNS") ||
		strings.HasPrefix(upper, "SHOW KEYS") || strings.HasPrefix(upper, "SHOW INDEX") ||
		strings.HasPrefix(upper, "SHOW VARIABLES") ||
		(strings.HasPrefix(upper, "SELECT") && strings.Contains(upper, "INFORMATION_SCHEMA")) ||
		(strings.HasPrefix(upper, "SELECT") && strings.Contains(upper, "@@COLLATION_DATABASE")):
		// Introspection queries used by dump tools (SHOW CREATE TABLE, information_schema
		// lookups, ...) are not supported; deny them with a real MySQL error so clients
		// fail cleanly instead of misreading responses.
		return nil, mysql.NewError(mysql.ER_TABLEACCESS_DENIED_ERROR,
			"SHOW command denied to user '"+h.username+"'@'localhost'")
	case strings.HasPrefix(upper, "SELECT"):
		return handleSelect(h.redis, h.currentDB, query)
	case strings.HasPrefix(upper, "CREATE DATABASE") || strings.HasPrefix(upper, "CREATE SCHEMA"):
		return handleCreateDatabase(h, query)
	case strings.HasPrefix(upper, "CREATE TEMPORARY TABLE") || strings.HasPrefix(upper, "CREATE TABLE"):
		return handleCreateTable(h, query)
	case strings.HasPrefix(upper, "DROP DATABASE") || strings.HasPrefix(upper, "DROP SCHEMA"):
		return handleDropDatabase(h, query)
	case strings.HasPrefix(upper, "DROP TEMPORARY TABLE") || strings.HasPrefix(upper, "DROP TABLE"):
		return handleDropTable(h, query)
	case strings.HasPrefix(upper, "TRUNCATE"):
		return handleTruncate(h, query)
	case strings.HasPrefix(upper, "DELETE"):
		return handleDelete(h, query)
	case strings.HasPrefix(upper, "INSERT") || strings.HasPrefix(upper, "REPLACE"):
		return handleInsert(h, query)
	case strings.HasPrefix(upper, "UPDATE"):
		return handleUpdate(h, query)
	case strings.HasPrefix(upper, "ALTER"):
		return handleAlter(h, query)
	case strings.HasPrefix(upper, "RENAME TABLE"):
		return handleRename(h, query)
	case strings.HasPrefix(upper, "GRANT") || strings.HasPrefix(upper, "REVOKE"):
		return handleGrant(h, query)
	default:
		return mysql.NewResult(nil), nil
	}
}

// HandleFieldList handles the COM_FIELD_LIST packet, used by clients to ask for the
// columns of a table. Not supported.
func (h *BlackHoleHandler) HandleFieldList(table string, fieldWildcard string) ([]*mysql.Field, error) {
	return nil, fmt.Errorf("not supported")
}

// HandleStmtPrepare handles the COM_STMT_PREPARE packet, the first step of prepared
// statement support. It is a no-op so clients can prepare statements without error.
func (h *BlackHoleHandler) HandleStmtPrepare(query string) (int, int, any, error) {
	return 0, 0, nil, nil
}

// HandleStmtExecute handles the COM_STMT_EXECUTE packet, which executes a previously
// prepared statement. It returns an empty result set.
func (h *BlackHoleHandler) HandleStmtExecute(context any, query string, args []any) (*mysql.Result, error) {
	return mysql.NewResult(nil), nil
}

// HandleStmtClose handles the COM_STMT_CLOSE packet, releasing a prepared statement.
// It's a no-op since prepared statements are not tracked.
func (h *BlackHoleHandler) HandleStmtClose(context any) error {
	return nil
}

// HandleOtherCommand handles any MySQL command not covered above (e.g. COM_SET_OPTION).
// Not supported.
func (h *BlackHoleHandler) HandleOtherCommand(cmd byte, data []byte) error {
	return fmt.Errorf("not supported")
}
