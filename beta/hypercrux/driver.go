// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/query"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The database/sql driver (G3), private to the package. Query, QueryRow,
// Exec and SQL() hand out database/sql's own values, and every statement
// reaches the engine through it:
//
//   - SQL() gives the DB's own handle, made in Open over connections bound
//     to the DB, and closed by Close. A statement through it carries nothing
//     in its context, so it runs through the database, and its errors wrap
//     neither ErrInvalid nor ErrNotFound, as through 0.x's SQL() (plain).
//   - Query, QueryRow and Exec go through calls, one handle for the whole
//     package, whose connections are bound to no database. Each statement's
//     context carries a via naming its DB, and its Tx when the call is
//     through one, so a Tx's statements run inside its Update and see its
//     changes. A pool limit a program sets on SQL() can't make them wait.
//
// database/sql turns each argument into a value with CheckNamedValue, by
// its usual conversion and then go-sqlite3's (argument). The driver parses
// the text, takes exactly one argument for each ? mark (bind), and runs the
// statement where its via puts it (via.run):
//
//   - a SELECT through the database in one read of the copy, and through a
//     Tx on the copy's transaction, with every row copied out before the
//     read ends, so open rows hold nobody up (drain);
//   - a write through the database in an Update of its own, and through a
//     Tx in its transaction, which goes back to where it was before the
//     statement when the statement fails (write).
//
// The engine runs a statement once the driver has it ready. Task G4 writes
// the Beta's; until then a statement that parses gives a stub's error.

// engine runs the statements that parse, with their arguments in the order
// of their ? marks, as Param.Index numbers them.
type engine interface {
	// query runs the SELECT s on r and gives the names of its result
	// columns and its rows. The driver reads every row, copying it, and
	// closes the rows before r's read ends, so the rows may read the store
	// as they go and use one buffer for every row.
	query(r store.Reader, s *query.Select, args []value.Value) (columns []string, rows query.Rows, err error)

	// write runs the INSERT, UPDATE or DELETE s in tx and gives the count
	// of rows it changed. When it fails, the driver takes back every change
	// it made, and the transaction carries on.
	write(tx *store.Tx, s query.Statement, args []value.Value) (changed int64, err error)
}

// notYetEngine is the engine until task G4 writes the Beta's.
type notYetEngine struct{}

func (notYetEngine) query(store.Reader, *query.Select, []value.Value) ([]string, query.Rows, error) {
	return nil, nil, notYet("SQL", "G4")
}

func (notYetEngine) write(*store.Tx, query.Statement, []value.Value) (int64, error) {
	return 0, notYet("SQL", "G4")
}

// via is what a call through the package's methods puts in its statement's
// context for the driver.
type via struct {
	db   *DB   // the database the call is on
	tx   *Tx   // the transaction the call is through, or nil
	fail error // an error to give at once, for failedRow
}

type viaKey struct{}

// context is the context of a statement through the database's methods.
func (db *DB) context() context.Context {
	return context.WithValue(context.Background(), viaKey{}, via{db: db})
}

// context is the context of a statement through the transaction, which
// the driver runs inside the transaction's Update.
func (t *Tx) context() context.Context {
	v := via{db: t.db, tx: t}
	if plant == "hypercrux/tx-dropped" {
		v.tx = nil
	}
	return context.WithValue(context.Background(), viaKey{}, v)
}

// calls is the handle the package's Query, QueryRow and Exec go through,
// for every DB. Its connections are bound to no database, since each
// statement's context names one. It's made on first use, since database/sql
// starts a goroutine for each handle, and lasts as long as the process.
var calls = sync.OnceValue(func() *sql.DB { return sql.OpenDB(connector{}) })

// closedSQL is what SQL gives on a DB that was never opened: a handle
// that's closed already, as a closed DB's is. Every call through it fails
// with database/sql's error for a closed handle, and a caller that sets it
// up first, with SetMaxOpenConns say, doesn't panic on a nil handle.
var closedSQL = sync.OnceValue(func() *sql.DB {
	s := sql.OpenDB(connector{})
	s.Close()
	return s
})

// failedRow returns a *sql.Row whose Scan gives err, for a QueryRow that
// fails before its statement is read, such as one on a closed database.
// Only database/sql can make a *sql.Row that holds an error, so the error
// goes through calls, in a via the driver gives back at once.
func failedRow(err error) *sql.Row {
	return calls().QueryRowContext(context.WithValue(context.Background(), viaKey{}, via{fail: err}), "")
}

// connector makes a handle's connections: bound to db for a DB's SQL(), and
// to no database for calls.
type connector struct{ db *DB }

func (c connector) Connect(context.Context) (driver.Conn, error) { return &conn{db: c.db}, nil }

func (connector) Driver() driver.Driver { return theDriver{} }

// theDriver is the driver.Driver behind every handle. database/sql calls
// its Open only for a name given to sql.Register, and the Beta registers
// none, so Open refuses.
type theDriver struct{}

func (theDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("hypercrux: the Beta's driver takes no data source name: open the database with Open and use its SQL()")
}

// conn is a connection. It holds nothing of its own, so database/sql can
// open and drop as many as it likes: what a statement needs is in its
// connection's DB, or in its context's via.
type conn struct{ db *DB }

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.NamedValueChecker  = (*conn)(nil)
)

// errBegin is Begin's error. Transactions go through Update, which takes
// the write lock and commits to the file, and BETA.md leaves SQL().Begin
// out.
var errBegin = fmt.Errorf("hypercrux: SQL().Begin is an %w in the Beta: a transaction goes through Update", errors.ErrUnsupported)

func (c *conn) Begin() (driver.Tx, error) { return nil, errBegin }

func (c *conn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) { return nil, errBegin }

func (c *conn) Close() error { return nil }

func (c *conn) Prepare(text string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), text)
}

// PrepareContext parses the statement once. database/sql then checks each
// call's count of arguments against its ? marks, through NumInput.
func (c *conn) PrepareContext(ctx context.Context, text string) (driver.Stmt, error) {
	s, err := query.Parse(text)
	if err != nil {
		return nil, err
	}
	return &stmt{c: c, s: s}, nil
}

func (c *conn) QueryContext(ctx context.Context, text string, args []driver.NamedValue) (driver.Rows, error) {
	out, _, err := c.statement(ctx, text, nil, args)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *conn) ExecContext(ctx context.Context, text string, args []driver.NamedValue) (driver.Result, error) {
	_, n, err := c.statement(ctx, text, nil, args)
	if err != nil {
		return nil, err
	}
	return result{changed: n}, nil
}

// CheckNamedValue turns each argument into its value as database/sql hands
// it over, so a statement gets value.Values. database/sql wraps an error
// from here as it wraps one from its own conversion, naming the argument.
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	v, err := argument(nv.Value)
	if err != nil {
		return err
	}
	nv.Value = v
	return nil
}

// statement runs one statement for the connection: the text, or s when it
// was parsed already, with its arguments. Through the methods its errors
// are as the engine gives them, and through SQL() they're plain.
func (c *conn) statement(ctx context.Context, text string, s query.Statement, args []driver.NamedValue) (*rows, int64, error) {
	v, methods := ctx.Value(viaKey{}).(via)
	if !methods {
		v = via{db: c.db}
	}
	out, n, err := v.statement(text, s, args)
	if err != nil && !methods {
		err = plain(err)
	}
	return out, n, err
}

// statement parses the text unless s holds it parsed already, binds the
// arguments and runs the statement where the via puts it.
func (v via) statement(text string, s query.Statement, args []driver.NamedValue) (*rows, int64, error) {
	if v.fail != nil {
		return nil, 0, v.fail
	}
	if s == nil {
		var err error
		if s, err = query.Parse(text); err != nil {
			return nil, 0, err
		}
	}
	vals, err := bind(s, args)
	if err != nil {
		return nil, 0, err
	}
	return v.run(s, vals)
}

// bind gives a statement's arguments in order, one for each ? mark. An
// argument with a name, from sql.Named, is refused, since the Beta's SQL
// has ? marks only and the arguments fill them in order, and so are fewer
// arguments than marks or more (SQL.md, "Arguments").
func bind(s query.Statement, args []driver.NamedValue) ([]value.Value, error) {
	for _, a := range args {
		if a.Name != "" {
			return nil, fmt.Errorf("hypercrux: the argument named %s has no mark to fill: the Beta's SQL has ? marks only, which the arguments fill in order", a.Name)
		}
	}
	if want := s.Params(); len(args) != want {
		noun := "arguments"
		if want == 1 {
			noun = "argument"
		}
		return nil, fmt.Errorf("hypercrux: the statement takes %d %s and was given %d", want, noun, len(args))
	}
	vals := make([]value.Value, len(args))
	for i, a := range args {
		v, ok := a.Value.(value.Value)
		if !ok {
			// Only a caller that reaches the connection itself, through
			// sql.Conn's Raw, can hand over an argument CheckNamedValue
			// hasn't seen.
			var err error
			if v, err = argument(a.Value); err != nil {
				return nil, err
			}
		}
		vals[i] = v
	}
	return vals, nil
}

// timeLayout is the layout go-sqlite3 writes a time.Time argument in, the
// first of its SQLiteTimestampFormats.
const timeLayout = "2006-01-02 15:04:05.999999999-07:00"

// argument turns a Go value given as an argument into its SQL value, as 0.x
// does through database/sql and go-sqlite3 (SQL.md, "Arguments").
// database/sql's usual conversion comes first: a driver.Valuer gives its
// Value, a nil pointer is NULL, another pointer gives what it points to,
// and named types go to their kinds, so whole numbers become an int64,
// apart from a uint64 of 2^63 or more, which it refuses, and float32
// widens. Other types are refused there. Then go-sqlite3's binding: nil is
// NULL, a whole number an integer, a bool 1 or 0, a float64 a real with NaN
// as NULL, a string text as given, a []byte bytes with a nil slice as NULL,
// and a time.Time text in timeLayout, in the time's own zone.
func argument(a any) (value.Value, error) {
	d, err := driver.DefaultParameterConverter.ConvertValue(a)
	if err != nil {
		return value.Value{}, err
	}
	switch x := d.(type) {
	case nil:
		return value.Null(), nil
	case int64:
		return value.Int(x), nil
	case bool:
		if x {
			return value.Int(1), nil
		}
		return value.Int(0), nil
	case float64:
		if math.IsNaN(x) {
			return value.Null(), nil
		}
		return value.Real(x), nil
	case string:
		return value.Text(x), nil
	case []byte:
		if x == nil {
			return value.Null(), nil
		}
		return value.Bytes(string(x)), nil
	case time.Time:
		layout := timeLayout
		if plant == "hypercrux/time-layout" {
			layout = time.RFC3339Nano
		}
		return value.Text(x.Format(layout)), nil
	}
	// database/sql passes on a decimal type that describes itself, which
	// go-sqlite3 can't bind either.
	return value.Value{}, fmt.Errorf("unsupported type %T", a)
}

// run runs a statement whose arguments are bound, where the via puts it,
// and gives a SELECT's rows or a write's count of changed rows.
func (v via) run(s query.Statement, args []value.Value) (*rows, int64, error) {
	db := v.db
	if db == nil {
		return nil, 0, fmt.Errorf("%w: a database that was never opened", ErrClosed)
	}
	e := db.engine
	sel, isSelect := s.(*query.Select)
	if v.tx != nil {
		stx, err := v.tx.open()
		if err != nil {
			return nil, 0, err
		}
		if isSelect {
			out, err := drain(e, stx, sel, args)
			return out, 0, err
		}
		return write(e, stx, s, args)
	}
	if isSelect {
		// One read, inside the copy's Read, which fails at once inside an
		// Update after its first change, as every read through db does.
		var out *rows
		err := db.read(func(r store.Reader) error {
			var err error
			out, err = drain(e, r, sel, args)
			return err
		})
		return out, 0, err
	}
	// A write through the database is an Update of its own, which fails at
	// once inside an Update, as every write through db does.
	var out *rows
	var n int64
	err := db.Update(func(tx *Tx) error {
		var err error
		out, n, err = write(e, tx.stx, s, args)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return out, n, nil
}

// write runs a write in tx through the engine. A statement is all or
// nothing, so when it fails, tx goes back to where it was before it and
// carries on (SQL.md, "Writes"). A write gives no columns and no rows to
// Query, as in 0.x.
func write(e engine, tx *store.Tx, s query.Statement, args []value.Value) (*rows, int64, error) {
	m := tx.Mark()
	n, err := e.write(tx, s, args)
	if err != nil {
		if plant != "hypercrux/write-kept" {
			tx.RollbackTo(m)
		}
		return nil, 0, err
	}
	return &rows{}, n, nil
}

// drain runs a SELECT on r through the engine and reads every row it gives
// into copies, then closes the rows, all before r's read ends. So the rows
// handed to database/sql hold nothing of the store's, and nobody waits for
// them. They read as the store was when the statement ran. Each row's
// values are copied out of the slice the engine hands over, which it may
// use again for the next row; the values themselves never change.
func drain(e engine, r store.Reader, s *query.Select, args []value.Value) (*rows, error) {
	cols, it, err := e.query(r, s, args)
	if err != nil {
		return nil, err
	}
	out := &rows{columns: cols}
	if plant == "hypercrux/rows-pulled-late" {
		out.late = it
		return out, nil
	}
	defer it.Close()
	for it.Next() {
		row := it.Row()
		if len(row) != len(cols) {
			return nil, fmt.Errorf("hypercrux: a row of %d values for %d columns", len(row), len(cols))
		}
		if plant == "hypercrux/row-kept" {
			out.kept = append(out.kept, row)
		}
		out.values = append(out.values, row...)
		out.count++
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// rows are a statement's rows as database/sql reads them, copied out of
// the store when the statement ran.
type rows struct {
	columns []string
	values  []value.Value // the rows' values, one row after another
	count   int           // how many rows
	next    int           // the row Next gives next

	kept [][]value.Value // the engine's own rows, kept only by a planted bug
	late query.Rows      // the engine's rows, read late only by a planted bug
}

var _ driver.Rows = (*rows)(nil)

func (r *rows) Columns() []string { return r.columns }

func (r *rows) Close() error {
	r.values, r.count = nil, 0
	return nil
}

func (r *rows) Next(dest []driver.Value) error {
	var row []value.Value
	switch {
	case r.late != nil:
		if !r.late.Next() {
			return io.EOF
		}
		row = r.late.Row()
	case r.next >= r.count:
		return io.EOF
	case r.kept != nil:
		row = r.kept[r.next]
	default:
		w := len(r.columns)
		row = r.values[r.next*w : r.next*w+w]
	}
	r.next++
	for i, v := range row {
		dest[i] = out(v)
	}
	return nil
}

// out gives a value to database/sql as 0.x's driver gives one: nil, an
// int64, a float64, a string, or a []byte of its own for bytes and for a
// vector, whose bytes are its values' bits, 4 bytes each, little-endian,
// the blob 0.x stores (P3). database/sql hands a []byte straight on to a
// sql.RawBytes or a Scanner, so each is a copy: nothing handed out shares
// the store's memory, and a caller that writes into one changes nothing
// else. SQL has no NaN, and a real that is NaN goes out as NULL, as SQLite
// would give it.
func out(v value.Value) driver.Value {
	switch v.Kind() {
	case value.KindInt:
		return v.Int()
	case value.KindReal:
		if f := v.Real(); !math.IsNaN(f) {
			return f
		}
	case value.KindText:
		return v.Text()
	case value.KindBytes, value.KindVector:
		if plant == "hypercrux/blob-shared" {
			return shared(v.Raw())
		}
		return []byte(v.Raw())
	}
	return nil
}

// result is an Exec's result.
type result struct{ changed int64 }

// RowsAffected gives the count of rows a write changed, and 0 for a SELECT.
func (r result) RowsAffected() (int64, error) { return r.changed, nil }

// LastInsertId returns an error, since records have no row numbers.
func (result) LastInsertId() (int64, error) { return 0, errLastInsertID }

var errLastInsertID = fmt.Errorf("hypercrux: LastInsertId is an %w in the Beta: records have no row numbers, and a key names each one", errors.ErrUnsupported)

// stmt is a prepared statement: its text, parsed once.
type stmt struct {
	c *conn
	s query.Statement
}

var (
	_ driver.Stmt             = (*stmt)(nil)
	_ driver.StmtQueryContext = (*stmt)(nil)
	_ driver.StmtExecContext  = (*stmt)(nil)
)

func (s *stmt) Close() error { return nil }

// NumInput is the statement's count of ? marks, which database/sql holds
// each call's arguments to.
func (s *stmt) NumInput() int { return s.s.Params() }

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	out, _, err := s.c.statement(ctx, "", s.s, args)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	_, n, err := s.c.statement(ctx, "", s.s, args)
	if err != nil {
		return nil, err
	}
	return result{changed: n}, nil
}

// Query and Exec are driver.Stmt's older methods, which database/sql calls
// only when a statement lacks the ones above.
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}

func named(args []driver.Value) []driver.NamedValue {
	nv := make([]driver.NamedValue, len(args))
	for i, a := range args {
		nv[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return nv
}

// plain gives err as a statement through SQL() gives it. 0.x hands out
// go-sqlite3's errors there as they are, so none wraps ErrInvalid or
// ErrNotFound, and each is of kind "error", even in an Exec. An error that
// wraps either of them becomes text, and a rule broken by a write reads as
// it does there in 0.x, without the "invalid: " that Exec's error has. The
// Beta's own errors, such as ErrInsideUpdate, are of kind "error" already,
// and keep their values.
func plain(err error) error {
	if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) || plant == "hypercrux/sql-wraps-invalid" {
		return err
	}
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, ErrInvalid.Error()+": "); ok {
		msg = "hypercrux: " + rest
	}
	return errors.New(msg)
}
