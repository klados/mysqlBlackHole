package main

import (
	"fmt"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"
)

type BlackHoleHandler struct {
	currentDB string
}

func (h *BlackHoleHandler) UseDB(dbName string) error {
	// toDo allow specific database names read them from redis
	h.currentDB = dbName
	return nil
}

func buildResult(columns []string, rows [][]any) (*mysql.Result, error) {
	r, err := mysql.BuildSimpleResultset(columns, rows, false)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(r), nil
}

func (h *BlackHoleHandler) HandleQuery(query string) (*mysql.Result, error) {
	q := strings.TrimSpace(query)
	upper := strings.ToUpper(q)

	switch {
	case upper == "SHOW DATABASES" || upper == "SHOW SCHEMAS":
		return buildResult(
			[]string{"Database"},
			[][]any{{"information_schema"}, {"mysql"}, {"test"}},
		)

	case strings.HasPrefix(upper, "SHOW TABLES"):
		tables := []any{"users", "orders", "products"}
		rows := make([][]any, len(tables))
		for i, t := range tables {
			rows[i] = []any{t}
		}
		colName := "Tables_in_" + h.currentDB
		if h.currentDB == "" {
			colName = "Tables_in_"
		}
		return buildResult([]string{colName}, rows)

	case upper == "SELECT DATABASE()" || upper == "SELECT DATABASE() AS `DATABASE()`":
		db := h.currentDB
		if db == "" {
			db = "NULL"
		}
		return buildResult(
			[]string{"DATABASE()"},
			[][]any{{db}},
		)

	case strings.HasPrefix(upper, "SELECT") && strings.Contains(upper, "VERSION"):
		return buildResult(
			[]string{"version()"},
			[][]any{{"5.7.0-blackhole"}},
		)

	case upper == "STATUS" || upper == "SELECT 1" || upper == "SELECT 1 AS `1`":
		return mysql.NewResult(nil), nil

	case strings.HasPrefix(upper, "SELECT"):
		return buildResult([]string{"result"}, [][]any{})

	default:
		return mysql.NewResult(nil), nil
	}
}

func (h *BlackHoleHandler) HandleFieldList(table string, fieldWildcard string) ([]*mysql.Field, error) {
	return nil, fmt.Errorf("not supported")
}

func (h *BlackHoleHandler) HandleStmtPrepare(query string) (int, int, any, error) {
	return 0, 0, nil, nil
}

func (h *BlackHoleHandler) HandleStmtExecute(context any, query string, args []any) (*mysql.Result, error) {
	return mysql.NewResult(nil), nil
}

func (h *BlackHoleHandler) HandleStmtClose(context any) error {
	return nil
}

func (h *BlackHoleHandler) HandleOtherCommand(cmd byte, data []byte) error {
	return fmt.Errorf("not supported")
}
