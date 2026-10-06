// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"fmt"
	"strings"
)

// schemaSQL creates HyperCrux's own tables and triggers. FORMAT.md explains
// each of them. Everything here is plain SQLite, so the rules hold for every
// program that writes the file, not only for this package.
var schemaSQL = []string{
	`CREATE TABLE IF NOT EXISTS hc_meta (
	name  TEXT PRIMARY KEY,
	value TEXT
) WITHOUT ROWID`,
	`INSERT OR IGNORE INTO hc_meta (name, value) VALUES ('format_version', '1')`,

	// One row per record table. dims is the vector size, set by the first
	// vector stored in the table.
	`CREATE TABLE IF NOT EXISTS hc_tables (
	name TEXT PRIMARY KEY,
	dims INTEGER
) WITHOUT ROWID`,

	// One row per record, kept by the record tables' triggers. Links point
	// at keys in here.
	`CREATE TABLE IF NOT EXISTS hc_keys (
	key TEXT PRIMARY KEY,
	tbl TEXT NOT NULL
) WITHOUT ROWID`,

	`CREATE TABLE IF NOT EXISTS hc_links (
	src  TEXT NOT NULL,
	type TEXT NOT NULL,
	dst  TEXT NOT NULL,
	PRIMARY KEY (src, type, dst)
) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS hc_links_in ON hc_links (dst, type, src)`,

	// A link needs both of its records, and a type.
	`CREATE TRIGGER IF NOT EXISTS hc_links_insert BEFORE INSERT ON hc_links
BEGIN
	SELECT RAISE(ABORT, 'hypercrux: a link type is text of 1 to 200 characters')
	WHERE typeof(NEW.type) <> 'text' OR length(NEW.type) NOT BETWEEN 1 AND 200;
	SELECT RAISE(ABORT, 'hypercrux: a link from a key that does not exist')
	WHERE NOT EXISTS (SELECT 1 FROM hc_keys WHERE key = NEW.src);
	SELECT RAISE(ABORT, 'hypercrux: a link to a key that does not exist')
	WHERE NOT EXISTS (SELECT 1 FROM hc_keys WHERE key = NEW.dst);
END`,
	`CREATE TRIGGER IF NOT EXISTS hc_links_update BEFORE UPDATE ON hc_links
BEGIN
	SELECT RAISE(ABORT, 'hypercrux: links are not changed in place; remove the link and add a new one');
END`,

	// When a record goes, its links go with it.
	`CREATE TRIGGER IF NOT EXISTS hc_keys_delete AFTER DELETE ON hc_keys
BEGIN
	DELETE FROM hc_links WHERE src = OLD.key;
	DELETE FROM hc_links WHERE dst = OLD.key;
END`,
	`CREATE TRIGGER IF NOT EXISTS hc_keys_update BEFORE UPDATE ON hc_keys
BEGIN
	SELECT RAISE(ABORT, 'hypercrux: hc_keys is kept by triggers and is not changed in place');
END`,
}

// recordTriggers returns the triggers that tie a record table to hc_keys:
// keys must start with the table's name and a colon, a new row registers its
// key, a deleted row takes its key (and so its links) away, and keys never
// change. The trigger names have dots in them, which table names can't, so
// no table's triggers can share a name with another's.
func recordTriggers(t string) []string {
	r := strings.NewReplacer("{T}", t, "{N}", fmt.Sprint(len(t)+1), "{MAX}", fmt.Sprint(MaxKeyLen))
	return []string{
		r.Replace(`CREATE TRIGGER IF NOT EXISTS "hc.{T}.insert" AFTER INSERT ON "{T}"
BEGIN
	SELECT RAISE(ABORT, 'hypercrux: keys in table {T} are text that starts with {T}:, up to {MAX} bytes')
	WHERE typeof(NEW.key) <> 'text' OR length(NEW.key) <= {N}
		OR substr(NEW.key, 1, {N}) <> '{T}:' COLLATE BINARY
		OR length(CAST(NEW.key AS BLOB)) > {MAX};
	INSERT OR IGNORE INTO hc_keys (key, tbl) VALUES (NEW.key, '{T}');
END`),
		r.Replace(`CREATE TRIGGER IF NOT EXISTS "hc.{T}.delete" AFTER DELETE ON "{T}"
BEGIN
	DELETE FROM hc_keys WHERE key = OLD.key;
END`),
		r.Replace(`CREATE TRIGGER IF NOT EXISTS "hc.{T}.key" BEFORE UPDATE OF key ON "{T}"
WHEN NEW.key IS NOT OLD.key COLLATE BINARY
BEGIN
	SELECT RAISE(ABORT, 'hypercrux: a key never changes; delete the record and put it again');
END`),
	}
}

// vecCheck sets the table's vector size from its first vector, then refuses
// any vector that isn't a blob of float32 values of that size, is larger
// than MaxDims, or is all zero bytes.
const vecCheck = `
	UPDATE hc_tables SET dims = length(NEW.vec) / 4
	WHERE name = '{T}' AND dims IS NULL AND typeof(NEW.vec) = 'blob'
		AND length(NEW.vec) > 0 AND length(NEW.vec) % 4 = 0 AND length(NEW.vec) <= {MAXB};
	SELECT RAISE(ABORT, 'hypercrux: vec must be a blob of float32 values, the same size in the whole table, and not all zero')
	WHERE typeof(NEW.vec) <> 'blob'
		OR length(NEW.vec) = 0
		OR length(NEW.vec) % 4 <> 0
		OR length(NEW.vec) > {MAXB}
		OR coalesce(length(NEW.vec) <> 4 * (SELECT dims FROM hc_tables WHERE name = '{T}'), 1)
		OR NEW.vec = zeroblob(length(NEW.vec));`

// vecTriggers returns the triggers that check the vec column. They exist
// only once the table has a vec column.
func vecTriggers(t string) []string {
	r := strings.NewReplacer("{T}", t, "{MAXB}", fmt.Sprint(4*MaxDims))
	check := r.Replace(vecCheck)
	return []string{
		r.Replace(`CREATE TRIGGER IF NOT EXISTS "hc.{T}.vec_insert" AFTER INSERT ON "{T}"
WHEN NEW.vec IS NOT NULL
BEGIN`) + check + "\nEND",
		r.Replace(`CREATE TRIGGER IF NOT EXISTS "hc.{T}.vec_update" AFTER UPDATE OF vec ON "{T}"
WHEN NEW.vec IS NOT NULL
BEGIN`) + check + "\nEND",
	}
}

// triggerNames lists the triggers a record table should have.
func triggerNames(t string, hasVec bool) []string {
	names := []string{"hc." + t + ".insert", "hc." + t + ".delete", "hc." + t + ".key"}
	if hasVec {
		names = append(names, "hc."+t+".vec_insert", "hc."+t+".vec_update")
	}
	return names
}

// tableInfo is what HyperCrux knows about one table, cached until the
// file's schema changes.
type tableInfo struct {
	exists   bool
	adopted  bool              // listed in hc_tables
	cols     map[string]string // lower-case name to the name as declared
	keyOK    bool              // has key TEXT PRIMARY KEY, alone
	strict   bool              // a STRICT table, whose new columns need a type
	triggers map[string]bool
}

func (ti *tableInfo) hasVec() bool { _, ok := ti.cols["vec"]; return ok }

// info returns what is known about table tbl inside this transaction. Once
// the transaction has changed the schema itself, it reads the file every
// time and leaves the cache alone, because the change may yet roll back.
func (t *Tx) info(tbl string) (*tableInfo, error) {
	if t.ddl {
		return readTableInfo(t.tx, tbl)
	}
	return t.db.info(t.run(), tbl)
}

// info returns what is known about table t, reading it from the file when
// the schema changed since the last look. The cache only ever holds what
// was read from committed schemas, so a schema_version number always means
// the same tables.
func (db *DB) info(q querier, t string) (*tableInfo, error) {
	var version int64
	if err := q.QueryRow(`PRAGMA schema_version`).Scan(&version); err != nil {
		return nil, err
	}
	db.mu.Lock()
	if version != db.schema {
		db.schema = version
		db.tables = nil
	}
	if ti := db.tables[t]; ti != nil {
		db.mu.Unlock()
		return ti, nil
	}
	db.mu.Unlock()

	ti, err := readTableInfo(q, t)
	if err != nil {
		return nil, err
	}
	db.mu.Lock()
	if db.schema == version {
		if db.tables == nil {
			db.tables = map[string]*tableInfo{}
		}
		db.tables[t] = ti
	}
	db.mu.Unlock()
	return ti, nil
}

// forget drops the cache. Update calls it after a transaction that changed
// the schema, whether it committed or not.
func (db *DB) forget() {
	db.mu.Lock()
	db.schema = -1
	db.tables = nil
	db.mu.Unlock()
}

// ddlExec runs a statement that changes the schema, and marks the
// transaction so it stops trusting the cache.
func (t *Tx) ddlExec(stmt string, args ...any) error {
	t.ddl = true
	_, err := t.tx.Exec(stmt, args...)
	return err
}

func readTableInfo(q querier, t string) (*tableInfo, error) {
	ti := &tableInfo{cols: map[string]string{}, triggers: map[string]bool{}}
	rows, err := q.Query(`SELECT name, type, pk FROM pragma_table_info(?)`, t)
	if err != nil {
		return nil, err
	}
	pks, keyText := 0, false
	for rows.Next() {
		var name, typ string
		var pk int
		if err := rows.Scan(&name, &typ, &pk); err != nil {
			rows.Close()
			return nil, err
		}
		ti.exists = true
		ti.cols[strings.ToLower(name)] = name
		if pk > 0 {
			pks++
		}
		if strings.EqualFold(name, "key") && pk > 0 && (strings.EqualFold(typ, "TEXT") || typ == "") {
			keyText = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ti.keyOK = keyText && pks == 1
	if !ti.exists {
		return ti, nil
	}
	var n int
	if err := q.QueryRow(`SELECT count(*) FROM hc_tables WHERE name = ?`, t).Scan(&n); err != nil {
		return nil, err
	}
	ti.adopted = n > 0
	if err := q.QueryRow(`SELECT strict FROM pragma_table_list WHERE schema = 'main' AND name = ?`, t).Scan(&ti.strict); err != nil {
		return nil, err
	}
	trows, err := q.Query(`SELECT name FROM sqlite_schema WHERE type = 'trigger' AND tbl_name = ?`, t)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	for trows.Next() {
		var name string
		if err := trows.Scan(&name); err != nil {
			return nil, err
		}
		ti.triggers[name] = true
	}
	return ti, trows.Err()
}

// ensure makes table t ready to take a record with the given fields: it
// creates or adopts the table, adds missing columns, and puts the vector
// triggers in place. It returns the table's column names for the fields.
func (t *Tx) ensure(tbl string, fields []string) (map[string]string, error) {
	ti, err := t.info(tbl)
	if err != nil {
		return nil, err
	}
	if !ti.exists {
		if err := t.ddlExec(`CREATE TABLE ` + quote(tbl) + ` (key TEXT PRIMARY KEY NOT NULL)`); err != nil {
			return nil, err
		}
	}
	if !ti.exists || !ti.adopted || !hasAll(ti.triggers, triggerNames(tbl, ti.hasVec())) {
		if err := t.adopt(tbl); err != nil {
			return nil, err
		}
		if ti, err = t.info(tbl); err != nil {
			return nil, err
		}
	}
	names := map[string]string{}
	vecAdded := false
	for _, f := range fields {
		lower := strings.ToLower(f)
		if have, ok := ti.cols[lower]; ok {
			names[f] = have
			continue
		}
		decl := quote(f)
		switch {
		case lower == "vec":
			decl += " BLOB"
			vecAdded = true
		case ti.strict:
			decl += " ANY"
		}
		if err := t.ddlExec(`ALTER TABLE ` + quote(tbl) + ` ADD COLUMN ` + decl); err != nil {
			return nil, err
		}
		names[f] = f
	}
	if vecAdded {
		if err := t.execAll(vecTriggers(tbl)); err != nil {
			return nil, err
		}
	}
	return names, nil
}

func hasAll(have map[string]bool, names []string) bool {
	for _, n := range names {
		if !have[n] {
			return false
		}
	}
	return true
}

// execAll runs statements that change the schema.
func (t *Tx) execAll(stmts []string) error {
	for _, s := range stmts {
		if err := t.ddlExec(s); err != nil {
			return err
		}
	}
	return nil
}

// Adopt makes an existing table a record table, for tables created with
// plain SQL. The table needs a column key that is its TEXT PRIMARY KEY, and
// every key must start with the table's name and a colon. Adopt registers
// the existing rows, checks any vectors in a vec column, and installs the
// triggers.
//
// Adopt also brings a record table back in step after its schema was
// changed with plain SQL, for example rebuilt by copying it to a new table:
// it reinstalls missing triggers, forgets keys whose rows are gone (and
// their links), and sets the vector size from the vectors in the table.
// Adopting a table that is in step does nothing.
func (db *DB) Adopt(table string) error {
	return db.Update(func(tx *Tx) error { return tx.Adopt(table) })
}

// Adopt is DB.Adopt inside a transaction.
func (t *Tx) Adopt(table string) error {
	if err := checkTable(table); err != nil {
		return err
	}
	return t.adopt(table)
}

func (t *Tx) adopt(tbl string) error {
	ti, err := readTableInfo(t.tx, tbl)
	if err != nil {
		return err
	}
	if !ti.exists {
		return fmt.Errorf("%w: no table %s (if it was dropped with plain SQL, Drop clears what's left of it)", ErrNotFound, tbl)
	}
	if !ti.keyOK {
		return fmt.Errorf("%w: table %s needs a column key that is its TEXT PRIMARY KEY", ErrInvalid, tbl)
	}
	if ti.adopted && hasAll(ti.triggers, triggerNames(tbl, ti.hasVec())) {
		return nil
	}
	prefix := tbl + ":"
	var bad int
	if err := t.tx.QueryRow(`SELECT count(*) FROM `+quote(tbl)+
		` WHERE typeof(key) <> 'text' OR length(key) <= ? OR substr(key, 1, ?) <> ? COLLATE BINARY
		OR length(CAST(key AS BLOB)) > ?`,
		len(prefix), len(prefix), prefix, MaxKeyLen).Scan(&bad); err != nil {
		return err
	}
	if bad > 0 {
		return fmt.Errorf("%w: %d keys in table %s don't start with %q or are longer than %d bytes", ErrInvalid, bad, tbl, prefix, MaxKeyLen)
	}
	t.ddl = true // hc_tables decides which tables count as records
	if _, err := t.tx.Exec(`INSERT OR IGNORE INTO hc_tables (name, dims) VALUES (?, NULL)`, tbl); err != nil {
		return err
	}
	// Keys whose rows went while the triggers were missing go too, and
	// their links with them.
	if _, err := t.tx.Exec(`DELETE FROM hc_keys WHERE tbl = ? AND key NOT IN (SELECT key FROM `+quote(tbl)+`)`, tbl); err != nil {
		return err
	}
	if _, err := t.tx.Exec(`INSERT OR IGNORE INTO hc_keys (key, tbl) SELECT key, ? FROM `+quote(tbl), tbl); err != nil {
		return err
	}
	dims := 0
	if ti.hasVec() {
		if dims, err = t.adoptVectors(tbl, ti.cols["vec"]); err != nil {
			return err
		}
	}
	var d any
	if dims > 0 {
		d = dims
	}
	if _, err := t.tx.Exec(`UPDATE hc_tables SET dims = ? WHERE name = ?`, d, tbl); err != nil {
		return err
	}
	if err := t.execAll(recordTriggers(tbl)); err != nil {
		return err
	}
	if ti.hasVec() {
		return t.execAll(vecTriggers(tbl))
	}
	return nil
}

// Drop deletes a record table: its records, their links, and the table. It
// also clears up after a record table that was dropped with plain SQL,
// whose keys and links would otherwise stay behind. Tables HyperCrux hasn't
// adopted are left alone.
func (db *DB) Drop(table string) error {
	return db.Update(func(tx *Tx) error { return tx.Drop(table) })
}

// Drop is DB.Drop inside a transaction.
func (t *Tx) Drop(table string) error {
	if err := checkTable(table); err != nil {
		return err
	}
	ti, err := readTableInfo(t.tx, table)
	if err != nil {
		return err
	}
	var listed int
	if err := t.tx.QueryRow(`SELECT count(*) FROM hc_tables WHERE name = ?`, table).Scan(&listed); err != nil {
		return err
	}
	switch {
	case listed == 0 && ti.exists:
		return fmt.Errorf("%w: %s isn't a record table; drop it with plain SQL", ErrInvalid, table)
	case listed == 0:
		return fmt.Errorf("%w: no record table %s", ErrNotFound, table)
	}
	t.ddl = true
	if ti.exists {
		if _, err := t.tx.Exec(`DELETE FROM ` + quote(table)); err != nil {
			return err
		}
		if _, err := t.tx.Exec(`DROP TABLE ` + quote(table)); err != nil {
			return err
		}
	}
	if _, err := t.tx.Exec(`DELETE FROM hc_keys WHERE tbl = ?`, table); err != nil {
		return err
	}
	_, err = t.tx.Exec(`DELETE FROM hc_tables WHERE name = ?`, table)
	return err
}

// adoptVectors checks the vectors already in a table and returns their
// size, or 0 when the table holds none.
func (t *Tx) adoptVectors(tbl, col string) (int, error) {
	rows, err := t.tx.Query(`SELECT key, ` + quote(col) + ` FROM ` + quote(tbl) + ` WHERE ` + quote(col) + ` IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	dims := 0
	for rows.Next() {
		var key string
		var raw any
		if err := rows.Scan(&key, &raw); err != nil {
			return 0, err
		}
		b, ok := raw.([]byte)
		if !ok {
			return 0, fmt.Errorf("%w: %s has a vec that isn't a blob", ErrInvalid, key)
		}
		v, err := DecodeVector(b)
		if err != nil {
			return 0, fmt.Errorf("%w: %s: %v", ErrInvalid, key, err)
		}
		if err := checkVector(v); err != nil {
			return 0, fmt.Errorf("%s: %w", key, err)
		}
		if dims == 0 {
			dims = len(v)
		} else if len(v) != dims {
			return 0, fmt.Errorf("%w: %s has %d values where the table's vectors have %d", ErrInvalid, key, len(v), dims)
		}
	}
	return dims, rows.Err()
}
