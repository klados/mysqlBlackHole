package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/pingcap/tidb/pkg/parser/ast"
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

// handleUse answers USE <db>, delegating to BlackHoleHandler.UseDB so the
// current database is tracked for subsequent queries. The database name keeps
// its original case since identifiers may be case-sensitive. It returns an
// empty result on success, matching MySQL's behavior.
func handleUse(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	fields := strings.Fields(query)
	if len(fields) < 2 {
		return mysql.NewResult(nil), nil
	}
	db := strings.TrimSuffix(fields[1], ";")
	if db == "" {
		return mysql.NewResult(nil), nil
	}
	if err := h.UseDB(db); err != nil {
		return nil, err
	}
	return mysql.NewResult(nil), nil
}

// handleSelectVersion answers SELECT VERSION(), returning a fixed MySQL
// server version string.
func handleSelectVersion() (*mysql.Result, error) {
	return buildResult(
		[]string{"version()"},
		[][]any{{"26.7.0 MySQL Community Server - GPL"}},
	)
}

// handleCreateDatabase answers CREATE DATABASE/CREATE SCHEMA. This black hole
// server never persists a database, so the statement always fails with an
// access-denied error, mirroring MySQL. The TiDB parser extracts the target
// database name so IF NOT EXISTS and charset/collation options are handled
// without affecting the denied name.
func handleCreateDatabase(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	dbName := ""

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		if create, ok := stmt.(*ast.CreateDatabaseStmt); ok {
			dbName = create.Name.O
		}
	}

	return nil, fmt.Errorf("Access denied for user '%s'@'%%' to database '%s'", h.username, dbName)
}

// handleDropDatabase answers DROP DATABASE/DROP SCHEMA. This black hole server
// never removes a database, so the statement always fails with an
// access-denied error, mirroring MySQL. The TiDB parser extracts the target
// database name so IF EXISTS is handled without affecting the denied name.
func handleDropDatabase(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	dbName := ""

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		if drop, ok := stmt.(*ast.DropDatabaseStmt); ok {
			dbName = drop.Name.O
		}
	}

	return nil, fmt.Errorf("Access denied for user '%s'@'%%' to database '%s'", h.username, dbName)
}

// handleDropTable answers DROP TABLE/DROP TEMPORARY TABLE. This black hole
// server never removes a table, so the statement always fails with an
// access-denied error, mirroring MySQL. If the table name has no dbschema
// qualification and no database is selected it returns MySQL's "No database
// selected" error instead. The TiDB parser extracts the target database (from
// a db.table qualification, falling back to the selected database) so IF EXISTS
// and TEMPORARY are handled without being persisted.
func handleDropTable(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	explicitDB := ""
	dbName := h.currentDB

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		if drop, ok := stmt.(*ast.DropTableStmt); ok && len(drop.Tables) > 0 {
			if s := drop.Tables[0].Schema.L; s != "" {
				explicitDB = s
			}
		}
	}

	if explicitDB == "" && dbName == "" {
		return nil, fmt.Errorf("No database selected")
	}
	if explicitDB != "" {
		dbName = explicitDB
	}

	return nil, fmt.Errorf("Access denied for user '%s'@'%%' to database '%s'", h.username, dbName)
}
func handleCreateTable(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	if h.currentDB == "" {
		return nil, fmt.Errorf("No database selected")
	}

	dbName := h.currentDB

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		if create, ok := stmt.(*ast.CreateTableStmt); ok && create.Table != nil {
			if create.Table.Schema.L != "" {
				dbName = create.Table.Schema.L
			}
		}
	}

	return nil, fmt.Errorf("Access denied for user '%s'@'%%' to database '%s'", h.username, dbName)
}
