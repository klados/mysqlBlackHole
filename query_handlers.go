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

// handleShowTables answers SHOW TABLES for the currently selected database, or
// the database named by SHOW TABLES FROM/IN <db> when given. It returns an
// error if no database is determined or the user is not allowed to access it.
func handleShowTables(rdb *redis.Client, dbName, username, query string) (*mysql.Result, error) {
	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		if show, ok := stmt.(*ast.ShowStmt); ok && show.DBName != "" {
			dbName = show.DBName
		}
	}

	if dbName == "" {
		return nil, fmt.Errorf("No database selected")
	}

	allowed, err := rdb.SIsMember(context.Background(), sqlemulate.GetSupportedDBUsersKey(dbName), username).Result()
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, fmt.Errorf("Access denied for user '%s' to database '%s'", username, dbName)
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
	tableName := ""
	dbName := h.currentDB

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		if drop, ok := stmt.(*ast.DropTableStmt); ok && len(drop.Tables) > 0 {
			tableName = drop.Tables[0].Name.L
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
	if res, err := requireTableInDB(h.redis, dbName, tableName); err != nil || res != nil {
		return res, err
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

// handleTruncate answers TRUNCATE [TABLE] <table>. This black hole server never
// empties a table, so the statement always fails with an access-denied error,
// mirroring MySQL. If the table name has no db schema qualification and no
// database is selected it returns MySQL's "No database selected" error instead.
func handleTruncate(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	explicitDB := ""
	tableName := ""
	dbName := h.currentDB

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		if tr, ok := stmt.(*ast.TruncateTableStmt); ok && tr.Table != nil {
			tableName = tr.Table.Name.L
			if s := tr.Table.Schema.L; s != "" {
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
	if res, err := requireTableInDB(h.redis, dbName, tableName); err != nil || res != nil {
		return res, err
	}

	return nil, fmt.Errorf("Access denied for user '%s'@'%%' to database '%s'", h.username, dbName)
}

// handleDelete answers DELETE [FROM] <table> [WHERE ...]. This black hole server
// never removes rows, so the statement always fails with an access-denied error,
// mirroring MySQL. If the target table has no db schema qualification and no
// database is selected it returns MySQL's "No database selected" error instead.
// Multi-table and joined deletes resolve to the selected database for the
// message; the statement is denied regardless.
func handleDelete(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	explicitDB := ""
	tableName := ""
	dbName := h.currentDB

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		if del, ok := stmt.(*ast.DeleteStmt); ok {
			if tbl := singleDeleteTable(del); tbl != nil {
				tableName = tbl.Name.L
				if s := tbl.Schema.L; s != "" {
					explicitDB = s
				}
			}
		}
	}

	resolvedDB := dbName
	if explicitDB != "" {
		resolvedDB = explicitDB
	}
	if res, err := requireTableInDB(h.redis, resolvedDB, tableName); err != nil || res != nil {
		return res, err
	}

	return denyForDB(h, explicitDB, dbName)
}

// handleInsert answers INSERT [INTO] <table> ... and REPLACE [INTO] <table> ...
// This black hole server never persists rows, so the statement always fails
// with an access-denied error, mirroring MySQL. If the target table has no db
// schema qualification and no database is selected it returns MySQL's "No
// database selected" error instead.
func handleInsert(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	explicitDB := ""
	tableName := ""
	dbName := h.currentDB

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		if ins, ok := stmt.(*ast.InsertStmt); ok {
			if tbl := singleTableFromRefs(ins.Table); tbl != nil {
				tableName = tbl.Name.L
				if s := tbl.Schema.L; s != "" {
					explicitDB = s
				}
			}
		}
	}

	resolvedDB := dbName
	if explicitDB != "" {
		resolvedDB = explicitDB
	}
	if res, err := requireTableInDB(h.redis, resolvedDB, tableName); err != nil || res != nil {
		return res, err
	}

	return denyForDB(h, explicitDB, dbName)
}

// handleUpdate answers UPDATE <table> SET ... This black hole server never
// modifies rows, so the statement always fails with an access-denied error,
// mirroring MySQL. If the target table has no db schema qualification and no
// database is selected it returns MySQL's "No database selected" error instead.
// Multi-table and joined updates resolve to the selected database for the
// message; the statement is denied regardless.
func handleUpdate(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	explicitDB := ""
	tableName := ""
	dbName := h.currentDB

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		if upd, ok := stmt.(*ast.UpdateStmt); ok {
			if tbl := singleTableFromRefs(upd.TableRefs); tbl != nil {
				tableName = tbl.Name.L
				if s := tbl.Schema.L; s != "" {
					explicitDB = s
				}
			}
		}
	}

	resolvedDB := dbName
	if explicitDB != "" {
		resolvedDB = explicitDB
	}
	if res, err := requireTableInDB(h.redis, resolvedDB, tableName); err != nil || res != nil {
		return res, err
	}

	return denyForDB(h, explicitDB, dbName)
}

// denyForDB resolves the database to report in an access-denied error. If no
// explicit (query-qualified) database and no selected database exist it returns
// MySQL's "No database selected" error, otherwise it denies access to the
// resolved database.
func denyForDB(h *BlackHoleHandler, explicitDB, selectedDB string) (*mysql.Result, error) {
	if explicitDB == "" && selectedDB == "" {
		return nil, fmt.Errorf("No database selected")
	}
	if explicitDB != "" {
		selectedDB = explicitDB
	}
	return nil, fmt.Errorf("Access denied for user '%s'@'%%' to database '%s'", h.username, selectedDB)
}

// tableExistsInDB reports whether the given table belongs to the database's
// allowed-tables set in Redis. The comparison is case-insensitive so that
// upper-cased seed names (e.g. INFORMATION_SCHEMA) match lower-cased
// identifiers produced by the SQL parser.
func tableExistsInDB(rdb *redis.Client, dbName, tableName string) (bool, error) {
	members, err := rdb.SMembers(context.Background(), sqlemulate.GetAllowedDBTablesKey(dbName)).Result()
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if strings.EqualFold(m, tableName) {
			return true, nil
		}
	}
	return false, nil
}

// requireTableInDB ensures the given table belongs to the resolved database,
// returning MySQL's ER_UNKNOWN_TABLE error when it does not. It returns the
// access result to return to the client (non-nil on error) or nil when the
// table is valid.
func requireTableInDB(rdb *redis.Client, dbName, tableName string) (*mysql.Result, error) {
	if dbName == "" || tableName == "" {
		return nil, nil
	}
	exists, err := tableExistsInDB(rdb, dbName, tableName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, mysql.NewError(mysql.ER_UNKNOWN_TABLE,
			fmt.Sprintf("Unknown table '%s' in %s", tableName, dbName))
	}
	return nil, nil
}

// handleAlter answers ALTER TABLE/ALTER DATABASE. This black hole server never
// modifies schema, so the statement always fails with an access-denied error,
// mirroring MySQL. The TiDB parser extracts the target database (from a
// db.table qualification, falling back to the selected database) so ALTER
// options are handled without being persisted.
func handleAlter(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	explicitDB := ""
	tableName := ""
	dbName := h.currentDB

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		switch s := stmt.(type) {
		case *ast.AlterTableStmt:
			if s.Table != nil {
				tableName = s.Table.Name.L
				if d := s.Table.Schema.L; d != "" {
					explicitDB = d
				}
			}
		case *ast.AlterDatabaseStmt:
			if s.Name.O != "" {
				explicitDB = s.Name.O
			}
		}
	}

	resolvedDB := dbName
	if explicitDB != "" {
		resolvedDB = explicitDB
	}
	if res, err := requireTableInDB(h.redis, resolvedDB, tableName); err != nil || res != nil {
		return res, err
	}

	return denyForDB(h, explicitDB, dbName)
}

// handleRename answers RENAME TABLE old TO new. This black hole server never
// renames a table, so the statement always fails with an access-denied error,
// mirroring MySQL. The TiDB parser extracts the database of the first table
// being renamed for the message.
func handleRename(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	explicitDB := ""
	tableName := ""
	dbName := h.currentDB

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		if rn, ok := stmt.(*ast.RenameTableStmt); ok && len(rn.TableToTables) > 0 {
			tableName = rn.TableToTables[0].OldTable.Name.L
			if s := rn.TableToTables[0].OldTable.Schema.L; s != "" {
				explicitDB = s
			}
		}
	}

	resolvedDB := dbName
	if explicitDB != "" {
		resolvedDB = explicitDB
	}
	if res, err := requireTableInDB(h.redis, resolvedDB, tableName); err != nil || res != nil {
		return res, err
	}

	return denyForDB(h, explicitDB, dbName)
}

// handleGrant answers GRANT/REVOKE privilege statements. This black hole server
// never grants or revokes privileges, so the statement always fails with an
// access-denied error, mirroring MySQL. The TiDB parser extracts the target
// database from the privilege level (falling back to the selected database).
func handleGrant(h *BlackHoleHandler, query string) (*mysql.Result, error) {
	explicitDB := ""
	dbName := h.currentDB

	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err == nil {
		switch s := stmt.(type) {
		case *ast.GrantStmt:
			if s.Level != nil && s.Level.DBName != "" {
				explicitDB = s.Level.DBName
			}
		case *ast.RevokeStmt:
			if s.Level != nil && s.Level.DBName != "" {
				explicitDB = s.Level.DBName
			}
		}
	}

	return denyForDB(h, explicitDB, dbName)
}

// singleDeleteTable unwraps the target of a single-table DELETE and returns the
// table name. Multi-table deletes and deletes with a join return nil.
func singleDeleteTable(del *ast.DeleteStmt) *ast.TableName {
	if del.TableRefs == nil {
		return nil
	}
	return singleTableFromRefs(del.TableRefs)
}

// singleTableFromRefs unwraps a single-table reference clause and returns its
// table name. References to multiple tables or a join return nil.
func singleTableFromRefs(tcl *ast.TableRefsClause) *ast.TableName {
	if tcl.TableRefs == nil {
		return nil
	}
	join := tcl.TableRefs
	if join.Right != nil {
		return nil
	}
	src, ok := join.Left.(*ast.TableSource)
	if !ok {
		return nil
	}
	tbl, ok := src.Source.(*ast.TableName)
	if !ok {
		return nil
	}
	return tbl
}
