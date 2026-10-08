// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

// reserved holds SQL.md's reserved words, which can't be bare names: the
// words SQLite won't read as a field in an expression.
var reserved = map[string]string{}

func init() {
	for _, w := range []string{
		"ADD", "ALL", "ALTER", "AND", "AS", "AUTOINCREMENT", "BETWEEN", "CASE", "CAST", "CHECK",
		"COLLATE", "COMMIT", "CONSTRAINT", "CREATE", "CURRENT_DATE", "CURRENT_TIME",
		"CURRENT_TIMESTAMP", "DEFAULT", "DEFERRABLE", "DELETE", "DISTINCT", "DROP", "ELSE",
		"ESCAPE", "EXCEPT", "EXISTS", "FOREIGN", "FROM", "GROUP", "HAVING", "IN", "INDEX", "INSERT",
		"INTERSECT", "INTO", "IS", "ISNULL", "JOIN", "LIMIT", "NOT", "NOTHING", "NOTNULL", "NULL", "ON",
		"OR", "ORDER", "PRIMARY", "RAISE", "REFERENCES", "RETURNING", "SELECT", "SET", "TABLE", "THEN",
		"TO", "TRANSACTION", "UNION", "UNIQUE", "UPDATE", "USING", "VALUES", "WHEN", "WHERE",
	} {
		reserved[w] = w
	}
}

// keyword reports whether a bare word is a reserved word, and gives it in
// upper case. Only ASCII letters and underscores make up a keyword.
func keyword(s string) (string, bool) {
	if len(s) < 2 || len(s) > 17 {
		return "", false
	}
	var b [17]byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z':
			c -= 'a' - 'A'
		case 'A' <= c && c <= 'Z' || c == '_':
		default:
			return "", false
		}
		b[i] = c
	}
	w, ok := reserved[string(b[:len(s)])]
	return w, ok
}

// The words that aren't reserved in the subset, and that SQLite reads as
// keywords in some of the places where the Beta takes a bare name. A bare
// name the Beta takes, SQLite takes as the same name.

// notBareAlias lists the words SQLite won't take as a bare alias, after a
// result column or after a source: its join keywords and INDEXED. LIKE,
// GLOB, REGEXP and MATCH can't be a result column's bare alias either,
// since SQLite reads them there as operators, which the parser does too.
var notBareAlias = map[string]bool{
	"cross": true, "full": true, "inner": true, "left": true, "natural": true, "outer": true,
	"right": true, "indexed": true,
}

// otherOperators are SQLite's operators written as words, outside the
// subset, which SQLite reads as operators right after an operand.
var otherOperators = map[string]bool{"glob": true, "regexp": true, "match": true}

// otherStatements are the words that start SQLite's other statements, for
// the message that refuses them.
var otherStatements = map[string]bool{
	"WITH": true, "VALUES": true, "EXPLAIN": true, "PRAGMA": true, "CREATE": true, "DROP": true,
	"ALTER": true, "REPLACE": true, "BEGIN": true, "COMMIT": true, "END": true, "ROLLBACK": true,
	"SAVEPOINT": true, "RELEASE": true, "ATTACH": true, "DETACH": true, "VACUUM": true,
	"ANALYZE": true, "REINDEX": true,
}

// funcInfo is a function of the subset: how many arguments it takes.
type funcInfo struct {
	min, max int // max is -1 for no limit
}

// functions are SQL.md's "Functions". min and max take one argument as
// aggregates and two or more otherwise, and count takes * or one.
var functions = map[string]funcInfo{
	"abs": {1, 1}, "coalesce": {2, -1}, "ifnull": {2, 2}, "instr": {2, 2}, "length": {1, 1},
	"lower": {1, 1}, "upper": {1, 1}, "max": {1, -1}, "min": {1, -1}, "nullif": {2, 2},
	"replace": {3, 3}, "round": {1, 2}, "substr": {2, 3}, "trim": {1, 2}, "typeof": {1, 1},
	"count": {1, 1}, "sum": {1, 1}, "total": {1, 1}, "avg": {1, 1},
	"distance": {2, 2}, "vector": {1, 1}, "walk": {2, 4},
	"date": {1, -1}, "datetime": {1, -1},
}

// aggregate reports whether a call to the function f with n arguments is an
// aggregate.
func aggregate(f string, n int, star bool) bool {
	switch f {
	case "count":
		return star || n == 1
	case "sum", "total", "avg":
		return true
	case "min", "max":
		return n == 1
	}
	return false
}

// sqliteFunctions are the functions SQLite 3.53.4 has, as go-sqlite3
// v1.14.52 builds it, from pragma_function_list. A call to one of them that
// the subset hasn't got is refused as outside the subset, and a call to a
// function neither has is "no such function", as SQLite says. The
// table-valued functions are here too.
var sqliteFunctions = map[string]bool{}

func init() {
	for _, f := range []string{
		"abs", "auth_enabled", "auth_user_add", "auth_user_change", "auth_user_delete",
		"authenticate", "avg", "changes", "char", "coalesce", "concat", "concat_ws", "count",
		"cume_dist", "current_date", "current_time", "current_timestamp", "date", "datetime",
		"dense_rank", "first_value", "format", "fts3_tokenizer", "glob", "group_concat", "hex", "if",
		"ifnull", "iif", "instr", "json", "json_array", "json_array_insert", "json_array_length",
		"json_error_position", "json_extract", "json_group_array", "json_group_object", "json_insert",
		"json_object", "json_patch", "json_pretty", "json_quote", "json_remove", "json_replace",
		"json_set", "json_type", "json_valid", "jsonb", "jsonb_array", "jsonb_array_insert",
		"jsonb_extract", "jsonb_group_array", "jsonb_group_object", "jsonb_insert", "jsonb_object",
		"jsonb_patch", "jsonb_remove", "jsonb_replace", "jsonb_set", "julianday", "lag",
		"last_insert_rowid", "last_value", "lead", "length", "like", "likelihood", "likely",
		"load_extension", "lower", "ltrim", "match", "matchinfo", "max", "min", "nth_value", "ntile",
		"nullif", "octet_length", "offsets", "optimize", "percent_rank", "printf", "quote", "random",
		"randomblob", "rank", "replace", "round", "row_number", "rtreecheck", "rtreedepth",
		"rtreenode", "rtrim", "sign", "snippet", "sqlite_compileoption_get",
		"sqlite_compileoption_used", "sqlite_log", "sqlite_source_id", "sqlite_version", "strftime",
		"string_agg", "substr", "substring", "subtype", "sum", "time", "timediff", "total",
		"total_changes", "trim", "typeof", "unhex", "unicode", "unistr", "unistr_quote", "unixepoch",
		"unlikely", "upper", "zeroblob",
		// table-valued
		"json_each", "json_tree", "jsonb_each", "jsonb_tree", "pragma_function_list",
	} {
		sqliteFunctions[f] = true
	}
}

// castTypes are the types CAST takes in the subset.
var castTypes = map[string]string{
	"integer": "INTEGER", "real": "REAL", "text": "TEXT", "blob": "BLOB", "numeric": "NUMERIC",
}

// SQLite's limits, which the Beta keeps, so a statement too big for 0.x is
// too big for the Beta.
const (
	maxHeight     = 1000  // SQLITE_MAX_EXPR_DEPTH
	maxColumns    = 2000  // SQLITE_MAX_COLUMN, for result columns, ORDER BY terms and SET
	maxArgs       = 1000  // SQLITE_MAX_FUNCTION_ARG
	maxParams     = 32766 // SQLITE_MAX_VARIABLE_NUMBER
	maxNesting    = 250   // the Beta's own: see parser.enter
	subqueryDepth = 10    // what a subquery counts for in maxNesting
)
