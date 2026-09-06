package main

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/opcode"
)

type evalCtx struct {
	columns []string
	row     []any
}

func (c *evalCtx) lookup(name string) (any, bool) {
	n := strings.ToLower(name)
	for i, col := range c.columns {
		if len(col) == len(n) && strings.ToLower(col) == n {
			return c.row[i], true
		}
	}
	return nil, false
}

type tri int

const (
	triFalse tri = iota
	triTrue
	triUnknown
)

func triOf(v any) tri {
	switch t := v.(type) {
	case nil:
		return triUnknown
	case bool:
		if t {
			return triTrue
		}
		return triFalse
	case int64:
		if t != 0 {
			return triTrue
		}
		return triFalse
	case int:
		if t != 0 {
			return triTrue
		}
		return triFalse
	case uint64:
		if t != 0 {
			return triTrue
		}
		return triFalse
	case float64:
		if t != 0 {
			return triTrue
		}
		return triFalse
	case string:
		if t != "" {
			return triTrue
		}
		return triFalse
	case []byte:
		if len(t) > 0 {
			return triTrue
		}
		return triFalse
	default:
		return triTrue
	}
}

func notTri(t tri) tri {
	switch t {
	case triTrue:
		return triFalse
	case triFalse:
		return triTrue
	default:
		return triUnknown
	}
}

// evalExpr evaluates the expression against the current row, returning a value
// any (nil for SQL NULL / unknown).
func evalExpr(c *evalCtx, e ast.ExprNode) (any, error) {
	switch n := e.(type) {
	case ast.ValueExpr:
		return n.GetValue(), nil
	case *ast.ColumnNameExpr:
		v, _ := c.lookup(n.Name.Name.L)
		return v, nil
	case *ast.ParenthesesExpr:
		return evalExpr(c, n.Expr)
	case *ast.BinaryOperationExpr:
		return evalBinary(c, n)
	case *ast.UnaryOperationExpr:
		return evalUnary(c, n)
	case *ast.IsNullExpr:
		v, err := evalExpr(c, n.Expr)
		if err != nil {
			return nil, err
		}
		isNull := v == nil
		if n.Not {
			isNull = !isNull
		}
		return isNull, nil
	case *ast.IsTruthExpr:
		v, err := evalExpr(c, n.Expr)
		if err != nil {
			return nil, err
		}
		tr := triOf(v)
		if n.Not {
			tr = notTri(tr)
		}
		if n.True > 0 {
			return tr == triTrue, nil
		}
		return tr == triFalse, nil
	case *ast.BetweenExpr:
		return evalBetween(c, n)
	case *ast.PatternInExpr:
		return evalIn(c, n)
	case *ast.PatternLikeOrIlikeExpr:
		return evalLike(c, n)
	case *ast.PatternRegexpExpr:
		return evalRegexp(c, n)
	case *ast.PositionExpr:
		return int64(n.N), nil
	default:
		return nil, nil
	}
}

func evalBinary(c *evalCtx, n *ast.BinaryOperationExpr) (any, error) {
	switch n.Op {
	case opcode.LogicAnd:
		l, err := evalExpr(c, n.L)
		if err != nil {
			return nil, err
		}
		if lt := triOf(l); lt == triFalse {
			return false, nil
		}
		r, err := evalExpr(c, n.R)
		if err != nil {
			return nil, err
		}
		rt := triOf(r)
		if rt == triFalse || rt == triUnknown {
			return rt == triTrue, nil
		}
		return true, nil
	case opcode.LogicOr:
		l, err := evalExpr(c, n.L)
		if err != nil {
			return nil, err
		}
		lt := triOf(l)
		if lt == triTrue {
			return true, nil
		}
		r, err := evalExpr(c, n.R)
		if err != nil {
			return nil, err
		}
		rt := triOf(r)
		if rt == triTrue {
			return true, nil
		}
		if lt == triUnknown || rt == triUnknown {
			return nil, nil
		}
		return false, nil
	case opcode.EQ, opcode.NE, opcode.LT, opcode.LE, opcode.GT, opcode.GE, opcode.NullEQ:
		l, err := evalExpr(c, n.L)
		if err != nil {
			return nil, err
		}
		r, err := evalExpr(c, n.R)
		if err != nil {
			return nil, err
		}
		return compareOp(n.Op, l, r)
	case opcode.Plus, opcode.Minus, opcode.Mul, opcode.Div, opcode.Mod:
		l, err := evalExpr(c, n.L)
		if err != nil {
			return nil, err
		}
		r, err := evalExpr(c, n.R)
		if err != nil {
			return nil, err
		}
		return arithmetic(n.Op, l, r)
	default:
		return nil, nil
	}
}

func evalUnary(c *evalCtx, n *ast.UnaryOperationExpr) (any, error) {
	v, err := evalExpr(c, n.V)
	if err != nil {
		return nil, err
	}

	switch n.Op {
	case opcode.Not, opcode.Not2:
		return notTri(triOf(v)) == triTrue, nil
	case opcode.Plus:
		return v, nil
	case opcode.Minus:
		if f, ok := toFloat(v); ok {
			return -f, nil
		}
		return nil, nil
	default:
		return nil, nil
	}
}

func evalBetween(c *evalCtx, n *ast.BetweenExpr) (any, error) {
	v, err := evalExpr(c, n.Expr)
	if err != nil {
		return nil, err
	}
	lo, err := evalExpr(c, n.Left)
	if err != nil {
		return nil, err
	}
	hi, err := evalExpr(c, n.Right)
	if err != nil {
		return nil, err
	}
	if v == nil || lo == nil || hi == nil {
		return nil, nil
	}
	inRange := compareValues(v, lo) >= 0 && compareValues(v, hi) <= 0
	if n.Not {
		inRange = !inRange
	}
	return inRange, nil
}

func evalIn(c *evalCtx, n *ast.PatternInExpr) (any, error) {
	v, err := evalExpr(c, n.Expr)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}

	matched := false
	hasNull := false
	for _, item := range n.List {
		iv, err := evalExpr(c, item)
		if err != nil {
			return nil, err
		}
		if iv == nil {
			hasNull = true
			continue
		}
		if compareValues(v, iv) == 0 {
			matched = true
		}
	}
	if !matched && hasNull {
		return nil, nil
	}
	res := matched
	if n.Not {
		res = !res
	}
	return res, nil
}

func evalLike(c *evalCtx, n *ast.PatternLikeOrIlikeExpr) (any, error) {
	v, err := evalExpr(c, n.Expr)
	if err != nil {
		return nil, err
	}
	pv, err := evalExpr(c, n.Pattern)
	if err != nil {
		return nil, err
	}
	if v == nil || pv == nil {
		return nil, nil
	}
	s := stringOfOr(v)
	re, err := likeToRegexp(stringOfOr(pv))
	if err != nil {
		return nil, nil
	}
	matched := re.MatchString(s)
	if n.Not {
		matched = !matched
	}
	return matched, nil
}

func evalRegexp(c *evalCtx, n *ast.PatternRegexpExpr) (any, error) {
	v, err := evalExpr(c, n.Expr)
	if err != nil {
		return nil, err
	}
	pv, err := evalExpr(c, n.Pattern)
	if err != nil {
		return nil, err
	}
	if v == nil || pv == nil {
		return nil, nil
	}
	re, err := regexp.Compile("(?i)" + stringOfOr(pv))
	if err != nil {
		return nil, nil
	}
	matched := re.MatchString(stringOfOr(v))
	if n.Not {
		matched = !matched
	}
	return matched, nil
}

func compareOp(op opcode.Op, l, r any) (any, error) {
	switch op {
	case opcode.NullEQ:
		if l == nil && r == nil {
			return true, nil
		}
		if l == nil || r == nil {
			return false, nil
		}
		return compareValues(l, r) == 0, nil
	case opcode.EQ, opcode.NE, opcode.LT, opcode.LE, opcode.GT, opcode.GE:
		if l == nil || r == nil {
			return nil, nil
		}
		cmp := compareValues(l, r)
		switch op {
		case opcode.EQ:
			return cmp == 0, nil
		case opcode.NE:
			return cmp != 0, nil
		case opcode.LT:
			return cmp < 0, nil
		case opcode.LE:
			return cmp <= 0, nil
		case opcode.GT:
			return cmp > 0, nil
		case opcode.GE:
			return cmp >= 0, nil
		}
	}
	return nil, nil
}

func arithmetic(op opcode.Op, l, r any) (any, error) {
	lf, lok := toFloat(l)
	rf, rok := toFloat(r)
	if !lok || !rok {
		return nil, nil
	}

	switch op {
	case opcode.Plus:
		return lf + rf, nil
	case opcode.Minus:
		return lf - rf, nil
	case opcode.Mul:
		return lf * rf, nil
	case opcode.Div:
		if rf == 0 {
			return nil, nil
		}
		return lf / rf, nil
	case opcode.Mod:
		if rf == 0 {
			return nil, nil
		}
		return math.Mod(lf, rf), nil
	}
	return nil, nil
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case uint64:
		return float64(t), true
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case nil:
		return 0, false
	default:
		return 0, false
	}
}

func stringOf(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case []byte:
		return string(t), true
	default:
		return "", false
	}
}

func stringOfOr(v any) string {
	if s, ok := stringOf(v); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func compareValues(a, b any) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}

	af, aIsNum := toFloat(a)
	bf, bIsNum := toFloat(b)
	if aIsNum && bIsNum {
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	}

	as := strings.ToLower(stringOfOr(a))
	bs := strings.ToLower(stringOfOr(b))
	return strings.Compare(as, bs)
}

func likeToRegexp(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString("(?i)")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '%':
			sb.WriteString(".*")
		case '_':
			sb.WriteString(".")
		case '\\':
			if i+1 < len(pattern) {
				i++
				sb.WriteString(regexp.QuoteMeta(string(pattern[i])))
			}
		default:
			sb.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	return regexp.Compile(sb.String())
}
