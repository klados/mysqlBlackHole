package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
	"github.com/redis/go-redis/v9"

	"mysqlBlackHole/model/sqlemulate"
)

/*
	SQL string
	↓
	TiDB parser
	↓
	validate SELECT statement
	↓
	extract one table name
	↓
	resolve database name
	↓
	check Redis: is table allowed?
	↓
	load table rows from Redis
	↓
	filter rows using WHERE
	↓
	sort rows using ORDER BY
	↓
	apply LIMIT/OFFSET
	↓
	project selected columns / expressions
	↓
	build MySQL protocol result
*/

var (
	selectParserOnce sync.Once
	selectParserMu   sync.Mutex
	selectParserVal  *parser.Parser
)

func getSelectParser() *parser.Parser {
	selectParserOnce.Do(func() {
		selectParserVal = parser.New()
	})
	return selectParserVal
}

func emptySelectResult() (*mysql.Result, error) {
	return buildResult([]string{"result"}, [][]any{})
}

// handleSelect answers SELECT statements against seeded tables. It parses the
// query with the tidb parser, loads the target table's seeded columns and rows
// from Redis, then filters, orders, limits and projects per the statement.
// Statements that can't be executed against seeded data return an empty result.
func handleSelect(rdb *redis.Client, currentDB, query string) (*mysql.Result, error) {
	selectParserMu.Lock()
	stmt, err := getSelectParser().ParseOneStmt(query, "", "")
	selectParserMu.Unlock()
	if err != nil {
		return emptySelectResult()
	}

	sel, ok := stmt.(*ast.SelectStmt)
	if !ok {
		return emptySelectResult()
	}

	table := singleTableName(sel)
	if table == nil {
		return emptySelectResult()
	}

	dbName := currentDB
	if table.Schema.L != "" {
		dbName = table.Schema.L
	}
	if dbName == "" {
		return emptySelectResult()
	}

	allowed, err := rdb.SIsMember(context.Background(), sqlemulate.GetAllowedDBTablesKey(dbName), table.Name.L).Result()
	if err != nil {
		return nil, err
	}
	if !allowed {
		return emptySelectResult()
	}

	data, found, err := loadTableData(rdb, dbName, table.Name.L)
	if err != nil {
		return nil, err
	}
	if !found {
		return emptySelectResult()
	}

	rows := filterRows(data.Columns, data.Rows, sel.Where)
	rows = orderRows(sel, data.Columns, rows)
	rows = applyLimit(rows, sel.Limit)

	outCols, outRows, err := projectRows(sel, data.Columns, rows)
	if err != nil {
		return emptySelectResult()
	}

	return buildResult(outCols, outRows)
}

func loadTableData(rdb *redis.Client, dbName, table string) (sqlemulate.TableData, bool, error) {
	val, err := rdb.Get(context.Background(), sqlemulate.GetTableDataKey(dbName, table)).Result()

	if err != nil {
		return sqlemulate.TableData{}, false, err
	}

	if errors.Is(err, redis.Nil) {
		return sqlemulate.TableData{}, false, nil
	}

	var data sqlemulate.TableData
	if err := json.Unmarshal([]byte(val), &data); err != nil {
		return sqlemulate.TableData{}, false, err
	}
	return data, true, nil
}

// singleTableName unwraps the FROM clause of a select and returns the table
// name when the query references exactly one table.
func singleTableName(sel *ast.SelectStmt) *ast.TableName {
	if sel.From == nil || sel.From.TableRefs == nil {
		return nil
	}
	join := sel.From.TableRefs
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

func filterRows(columns []string, rows [][]any, where ast.ExprNode) [][]any {
	if where == nil {
		return rows
	}

	out := make([][]any, 0, len(rows))
	for _, row := range rows {
		ctx := &evalCtx{columns: columns, row: row}
		v, err := evalExpr(ctx, where)
		if err != nil {
			continue
		}
		if triOf(v) == triTrue {
			out = append(out, row)
		}
	}
	return out
}

func orderRows(sel *ast.SelectStmt, columns []string, rows [][]any) [][]any {
	if sel.OrderBy == nil {
		return rows
	}

	sort.SliceStable(rows, func(i, j int) bool {
		iCtx := &evalCtx{columns: columns, row: rows[i]}
		jCtx := &evalCtx{columns: columns, row: rows[j]}
		for _, item := range sel.OrderBy.Items {
			expr := item.Expr
			if pos, ok := expr.(*ast.PositionExpr); ok {
				if col := nthSelectColumn(sel, pos.N); col != "" {
					expr = &ast.ColumnNameExpr{Name: &ast.ColumnName{Name: ast.NewCIStr(col)}}
				} else {
					continue
				}
			}
			vi, _ := evalExpr(iCtx, expr)
			vj, _ := evalExpr(jCtx, expr)
			if cmp := compareValues(vi, vj); cmp != 0 {
				if item.Desc {
					return cmp > 0
				}
				return cmp < 0
			}
		}
		return false
	})
	return rows
}

// nthSelectColumn resolves ORDER BY N to the name of the Nth selected column.
func nthSelectColumn(sel *ast.SelectStmt, n int) string {
	if n < 1 || sel.Fields == nil || n > len(sel.Fields.Fields) {
		return ""
	}
	ce, ok := sel.Fields.Fields[n-1].Expr.(*ast.ColumnNameExpr)
	if !ok {
		return ""
	}
	return ce.Name.Name.O
}

func applyLimit(rows [][]any, lim *ast.Limit) [][]any {
	if lim == nil {
		return rows
	}

	offset := 0
	if lim.Offset != nil {
		if v, ok := literalInt(lim.Offset); ok && v > 0 {
			offset = v
		}
	}
	if offset >= len(rows) {
		return nil
	}
	rows = rows[offset:]

	if lim.Count != nil {
		if n, ok := literalInt(lim.Count); ok && n >= 0 {
			if n < len(rows) {
				rows = rows[:n]
			}
		}
	}
	return rows
}

func literalInt(e ast.ExprNode) (int, bool) {
	ve, ok := e.(ast.ValueExpr)
	if !ok {
		return 0, false
	}
	switch v := ve.GetValue().(type) {
	case int64:
		return int(v), true
	case uint64:
		return int(v), true
	case int:
		return v, true
	default:
		return 0, false
	}
}

func projectRows(sel *ast.SelectStmt, dataCols []string, rows [][]any) ([]string, [][]any, error) {
	type proj struct {
		col  string
		idx  int
		expr ast.ExprNode
	}

	var projs []proj
	for _, f := range sel.Fields.Fields {
		switch {
		case f.WildCard != nil:
			for i, col := range dataCols {
				projs = append(projs, proj{col: col, idx: i})
			}
		case f.AsName.O != "":
			idx, _ := columnIndex(dataCols, f.AsName.O)
			_, isCol := f.Expr.(*ast.ColumnNameExpr)
			if isCol {
				projs = append(projs, proj{col: f.AsName.O, idx: idx})
			} else {
				projs = append(projs, proj{col: f.AsName.O, idx: -1, expr: f.Expr})
			}
		default:
			if ce, ok := f.Expr.(*ast.ColumnNameExpr); ok {
				idx, _ := columnIndex(dataCols, ce.Name.Name.L)
				projs = append(projs, proj{col: ce.Name.Name.O, idx: idx})
			} else {
				projs = append(projs, proj{col: "?column?", idx: -1, expr: f.Expr})
			}
		}
	}

	outCols := make([]string, len(projs))
	for i, p := range projs {
		outCols[i] = p.col
	}

	outRows := make([][]any, len(rows))
	for ri, row := range rows {
		ctx := &evalCtx{columns: dataCols, row: row}
		out := make([]any, len(projs))
		for pi, p := range projs {
			if p.idx >= 0 {
				out[pi] = normalizeValue(row[p.idx])
			} else if p.expr != nil {
				v, _ := evalExpr(ctx, p.expr)
				out[pi] = normalizeValue(v)
			}
		}
		outRows[ri] = out
	}
	return outCols, outRows, nil
}

func columnIndex(columns []string, name string) (int, bool) {
	n := strings.ToLower(name)
	for i, col := range columns {
		if len(col) == len(n) && strings.ToLower(col) == n {
			return i, true
		}
	}
	return -1, false
}

// normalizeValue converts values read from JSON (or produced by evaluation)
// into types the MySQL text protocol can encode: integral floats become int64,
// bools become 0/1.
func normalizeValue(v any) any {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) {
			return int64(t)
		}
		return t
	case bool:
		if t {
			return int64(1)
		}
		return int64(0)
	default:
		return v
	}
}
