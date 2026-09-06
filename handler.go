package main

import (
	"context"
	"fmt"
	"mysqlBlackHole/model/sqlemulate"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/redis/go-redis/v9"
)

type BlackHoleHandler struct {
	currentDB string
	username  string
	redis     *redis.Client
}

// SetUsername stores the authenticated username so DB access can be scoped per user.
func (h *BlackHoleHandler) SetUsername(username string) {
	h.username = username
}

// UseDB handles the COM_INIT_DB packet, sent when a client executes "USE <db>".
// It validates the authenticated user is allowed to use the database and tracks
// it as the current DB.
func (h *BlackHoleHandler) UseDB(dbName string) error {
	allowed, err := h.redis.SIsMember(context.Background(), sqlemulate.GetSupportedDBUsersKey(dbName), h.username).Result()
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("Access denied for user '%s' to database '%s'", h.username, dbName)
	}

	h.currentDB = dbName
	return nil
}

// HandleQuery handles the COM_QUERY packet, covering ordinary SQL statements such as
// SELECT, SHOW, and others. It inspects the query and delegates to the matching
// helper in query_handlers.go.
func (h *BlackHoleHandler) HandleQuery(query string) (*mysql.Result, error) {
	upper := strings.ToUpper(strings.TrimSpace(query))

	switch {
	case upper == "SHOW DATABASES" || upper == "SHOW SCHEMAS":
		return handleShowDatabases(h.redis, h.username)
	case strings.HasPrefix(upper, "SHOW TABLES"):
		return handleShowTables(h.redis, h.currentDB)
	case upper == "SELECT DATABASE()" || upper == "SELECT DATABASE() AS `DATABASE()`":
		return handleSelectDatabase(h.currentDB)
	case strings.HasPrefix(upper, "SELECT") && strings.Contains(upper, "VERSION"):
		return handleSelectVersion()
	case strings.HasPrefix(upper, "SELECT"):
		return handleSelect(h.redis, h.currentDB, query)
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
