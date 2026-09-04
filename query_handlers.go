package main

import (
	"context"
	"fmt"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/redis/go-redis/v9"

	"mysqlBlackHole/model/sqlemulate"
)

// buildResult constructs a *mysql.Result from the given column names and rows,
// returning an error if the resultset can't be built.
func buildResult(columns []string, rows [][]any) (*mysql.Result, error) {
	r, err := mysql.BuildSimpleResultset(columns, rows, false)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(r), nil
}

// handleShowDatabases answers SHOW DATABASES. It returns only the databases the
// given username is allowed to access, where databases are stored as a Redis
// set of names keyed by user.
func handleShowDatabases(rdb *redis.Client, username string) (*mysql.Result, error) {
	dbNames, err := rdb.SMembers(context.Background(), sqlemulate.KeySupportedDBs).Result()
	if err != nil {
		return nil, err
	}

	rows := make([][]any, 0, len(dbNames))
	for _, db := range dbNames {
		allowed, err := rdb.SIsMember(context.Background(), sqlemulate.GetSupportedDBUsersKey(db), username).Result()
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		rows = append(rows, []any{db})
	}

	return buildResult([]string{"Database"}, rows)
}

// handleShowTables answers SHOW TABLES for the currently selected database.
// It returns an error if no database is selected or the database has no tables.
func handleShowTables(rdb *redis.Client, dbName string) (*mysql.Result, error) {

	if dbName == "" {
		return nil, fmt.Errorf("No database selected")
	}

	tables, err := rdb.SMembers(context.Background(), fmt.Sprintf("%s:%s:tables", sqlemulate.KeySupportedDBs, dbName)).Result()

	if err != nil {
		return nil, err
	}

	if len(tables) == 0 {
		return nil, fmt.Errorf("No tables found")
	}

	rows := make([][]any, len(tables))
	for i, t := range tables {
		rows[i] = []any{t}
	}
	colName := "Tables_in_" + dbName
	return buildResult([]string{colName}, rows)
}

// handleSelectDatabase answers SELECT DATABASE(), returning the currently
// selected database name. When no database is selected it returns the string
// "NULL", approximating MySQL's SQL NULL.
func handleSelectDatabase(dbName string) (*mysql.Result, error) {
	db := dbName
	if db == "" {
		db = "NULL"
	}
	return buildResult([]string{"DATABASE()"}, [][]any{{db}})
}

// handleSelectVersion answers SELECT VERSION(), returning a fixed MySQL
// server version string.
func handleSelectVersion() (*mysql.Result, error) {
	return buildResult(
		[]string{"version()"},
		[][]any{{"26.7.0 MySQL Community Server - GPL"}},
	)
}

// handleSelectFallback is the catch-all handler for other SELECT statements,
// returning an empty resultset.
func handleSelectFallback() (*mysql.Result, error) {
	return buildResult([]string{"result"}, [][]any{})
}
