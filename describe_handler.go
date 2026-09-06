package main

import (
	"context"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/redis/go-redis/v9"

	"mysqlBlackHole/model/sqlemulate"
)

// describeColumns lists the result columns of a MySQL DESCRIBE statement in
// the same order real MySQL returns them.
var describeColumns = []string{"Field", "Type", "Null", "Key", "Default", "Extra"}

// handleDescribe answers DESCRIBE/DESC <table>. It returns one row per seeded
// column with the column name, declared type and a schema summary, mirroring
// MySQL's DESCRIBE result shape. Tables without seeded metadata yield an empty
// result set.
func handleDescribe(rdb *redis.Client, currentDB, query string) (*mysql.Result, error) {
	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err != nil {
		return emptySelectResult()
	}

	explain, ok := stmt.(*ast.ExplainStmt)
	if !ok {
		return emptySelectResult()
	}
	show, ok := explain.Stmt.(*ast.ShowStmt)
	if !ok || show.Tp != ast.ShowColumns || show.Table == nil {
		return emptySelectResult()
	}

	dbName := currentDB
	if show.Table.Schema.L != "" {
		dbName = show.Table.Schema.L
	}
	if dbName == "" {
		return emptySelectResult()
	}

	allowed, err := rdb.SIsMember(context.Background(), sqlemulate.GetAllowedDBTablesKey(dbName), show.Table.Name.L).Result()
	if err != nil {
		return nil, err
	}
	if !allowed {
		return emptySelectResult()
	}

	data, found, err := loadTableData(rdb, dbName, show.Table.Name.L)
	if err != nil {
		return nil, err
	}
	if !found {
		return emptySelectResult()
	}

	rows := make([][]any, 0, len(data.Columns))
	for _, c := range data.Columns {
		null, key := "YES", ""
		if strings.EqualFold(c.Name, "id") {
			null, key = "NO", "PRI"
		}
		rows = append(rows, []any{c.Name, c.Type, null, key, nil, ""})
	}

	return buildResult(describeColumns, rows)
}
