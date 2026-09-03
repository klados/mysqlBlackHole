package main

import "github.com/go-mysql-org/go-mysql/mysql"

func buildResult(columns []string, rows [][]any) (*mysql.Result, error) {
	r, err := mysql.BuildSimpleResultset(columns, rows, false)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(r), nil
}

func handleShowDatabases() (*mysql.Result, error) {
	return buildResult(
		[]string{"Database"},
		[][]any{{"information_schema"}, {"mysql"}, {"test"}},
	)
}

func handleShowTables(dbName string) (*mysql.Result, error) {
	tables := []any{"users", "orders", "products"}
	rows := make([][]any, len(tables))
	for i, t := range tables {
		rows[i] = []any{t}
	}
	colName := "Tables_in_" + dbName
	if dbName == "" {
		colName = "Tables_in_"
	}
	return buildResult([]string{colName}, rows)
}

func handleSelectDatabase(dbName string) (*mysql.Result, error) {
	db := dbName
	if db == "" {
		db = "NULL"
	}
	return buildResult([]string{"DATABASE()"}, [][]any{{db}})
}

func handleSelectVersion() (*mysql.Result, error) {
	return buildResult(
		[]string{"version()"},
		[][]any{{"5.7.0-blackhole"}},
	)
}

func handleSelectFallback() (*mysql.Result, error) {
	return buildResult([]string{"result"}, [][]any{})
}
