// Package store is what the Hub's store and the agent's store share:
// one SQLite database per end, and the stored form of a node, which both
// ends keep. The decisions behind it are in "Define what the Hub and the
// agent persist",
// recorded as ADR 0010.
//
// A database holds tables of entities, one row each: a key, the row's
// position in its table, and a JSON body. Its owner loads every table
// once when it opens it, keeps the entities in memory, and saves them
// with Commit, one transaction per event, which writes only the rows
// whose body or position changed. The database is the pure-Go
// modernc.org/sqlite, in WAL mode with synchronous=FULL, so a commit that
// has returned survives a power loss.
package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	// The pure-Go SQLite driver, registered as "sqlite": Silkpool builds
	// without cgo.
	_ "modernc.org/sqlite"
)

// A Migration moves a database from the schema version before it to its
// own. The first creates version 1 in an empty database, so a schema's
// version is the number of its migrations. There are no down-migrations.
type Migration func(*sql.Tx) error

// TooNew refuses a database that a newer Silkpool wrote: its schema
// version is Version, and this one reads up to Supported. Neither the
// Hub nor an agent goes back a version. Fix is what the operator can do
// instead, which the database's owner gives Open: a copy of the state
// folder is a way back for the Hub, but not for a node.
type TooNew struct {
	Path               string
	Version, Supported int
	Fix                string
}

func (e TooNew) Error() string {
	return fmt.Sprintf("the database %s has schema version %d, and this Silkpool reads only up to version %d: "+
		"a newer Silkpool wrote it, so %s",
		e.Path, e.Version, e.Supported, e.Fix)
}

// Unusable refuses a database that can't be read, or that fails SQLite's
// quick_check. Nothing repairs or resets it.
type Unusable struct {
	Path string
	Err  error
}

func (e Unusable) Error() string {
	return fmt.Sprintf("the database %s is corrupt or unreadable, and Silkpool won't repair or reset it: %v", e.Path, e.Err)
}

func (e Unusable) Unwrap() error { return e.Err }

// A DB is one end's database, open for that end alone: a service is the
// only writer of its database.
type DB struct {
	path string
	db   *sql.DB
	// conn is the one connection every statement goes through, so the
	// settings made when it opened hold for every commit.
	conn *sql.Conn
	// written holds each table's rows as the database holds them, by
	// key, as last loaded or committed.
	written map[string]map[string]placed
}

// A Row is one entity in a table: its key, unique in the table, and its
// stored form as JSON.
type Row struct {
	Key  string
	Body []byte
}

// placed is a row as the database holds it, at its position.
type placed struct {
	pos  int
	body []byte
}

// Open opens the database at path, creating it if it doesn't exist, and
// migrates it forward to the schema migrations make, in one transaction.
// It refuses a database that fails quick_check or can't be read, as
// Unusable, and one a newer schema wrote, as TooNew with tooNew as its
// Fix, in both cases without changing it.
func Open(path string, migrations []Migration, tooNew string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, Unusable{Path: path, Err: err}
	}
	d := &DB{path: path, db: db, written: map[string]map[string]placed{}}
	if err := d.open(migrations, tooNew); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) open(migrations []Migration, tooNew string) error {
	ctx := context.Background()
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return Unusable{Path: d.path, Err: err}
	}
	d.conn = conn
	if err := d.check(); err != nil {
		return Unusable{Path: d.path, Err: err}
	}
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return Unusable{Path: d.path, Err: err}
	}
	if version > len(migrations) {
		return TooNew{Path: d.path, Version: version, Supported: len(migrations), Fix: tooNew}
	}
	for _, p := range []string{"PRAGMA journal_mode = WAL", "PRAGMA synchronous = FULL"} {
		if _, err := conn.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("opening the database %s: %s: %w", d.path, p, err)
		}
	}
	if version < len(migrations) {
		return d.migrate(version, migrations)
	}
	return nil
}

// check runs quick_check, which reads every page.
func (d *DB) check() error {
	rows, err := d.conn.QueryContext(context.Background(), "PRAGMA quick_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("quick_check: %s", strings.Join(problems, "; "))
	}
	return nil
}

// migrate runs the migrations after version, and records the new
// version, in one transaction.
func (d *DB) migrate(version int, migrations []Migration) error {
	tx, err := d.conn.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("migrating the database %s: %w", d.path, err)
	}
	defer tx.Rollback()
	for v := version; v < len(migrations); v++ {
		if err := migrations[v](tx); err != nil {
			return fmt.Errorf("migrating the database %s to schema version %d: %w", d.path, v+1, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return fmt.Errorf("migrating the database %s: %w", d.path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrating the database %s: %w", d.path, err)
	}
	return nil
}

// Path returns the database's file.
func (d *DB) Path() string { return d.path }

// Close closes the database.
func (d *DB) Close() error {
	if d.conn != nil {
		d.conn.Close()
	}
	return d.db.Close()
}

// Load returns the rows of table, in order.
func (d *DB) Load(table string) ([]Row, error) {
	rows, err := d.conn.QueryContext(context.Background(), "SELECT key, body FROM "+table+" ORDER BY pos")
	if err != nil {
		return nil, fmt.Errorf("loading %s from %s: %w", table, d.path, err)
	}
	defer rows.Close()
	var out []Row
	written := map[string]placed{}
	for rows.Next() {
		var r Row
		var body string
		if err := rows.Scan(&r.Key, &body); err != nil {
			return nil, fmt.Errorf("loading %s from %s: %w", table, d.path, err)
		}
		r.Body = []byte(body)
		written[r.Key] = placed{pos: len(out), body: r.Body}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loading %s from %s: %w", table, d.path, err)
	}
	d.written[table] = written
	return out, nil
}

// Query runs a query that reads the database, such as one of the Hub's
// history.
func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.conn.QueryContext(context.Background(), query, args...)
}

// A Tx is one commit under way.
type Tx struct {
	d       *DB
	tx      *sql.Tx
	written map[string]map[string]placed
}

// Commit runs f in one transaction and commits it. If f or the commit
// fails, nothing f did is kept, on disk or in what d holds as written.
func (d *DB) Commit(f func(*Tx) error) error {
	tx, err := d.conn.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("committing to %s: %w", d.path, err)
	}
	defer tx.Rollback()
	t := &Tx{d: d, tx: tx, written: map[string]map[string]placed{}}
	if err := f(t); err != nil {
		return fmt.Errorf("committing to %s: %w", d.path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing to %s: %w", d.path, err)
	}
	for table, rows := range t.written {
		d.written[table] = rows
	}
	return nil
}

// Save makes rows, in order, the whole of table: it writes each row
// whose body or position changed, and deletes each row whose key isn't
// among them.
func (t *Tx) Save(table string, rows []Row) error {
	old, ok := t.written[table]
	if !ok {
		old = t.d.written[table]
	}
	next := make(map[string]placed, len(rows))
	for pos, r := range rows {
		if _, ok := next[r.Key]; ok {
			return fmt.Errorf("saving %s: two rows have the key %q", table, r.Key)
		}
		next[r.Key] = placed{pos: pos, body: slices.Clone(r.Body)}
		was, ok := old[r.Key]
		if ok && was.pos == pos && bytes.Equal(was.body, r.Body) {
			continue
		}
		if _, err := t.tx.Exec("INSERT INTO "+table+" (key, pos, body) VALUES (?, ?, ?) "+
			"ON CONFLICT (key) DO UPDATE SET pos = excluded.pos, body = excluded.body",
			r.Key, pos, string(r.Body)); err != nil {
			return fmt.Errorf("saving %s %q: %w", table, r.Key, err)
		}
	}
	for key := range old {
		if _, ok := next[key]; ok {
			continue
		}
		if _, err := t.tx.Exec("DELETE FROM "+table+" WHERE key = ?", key); err != nil {
			return fmt.Errorf("deleting %s %q: %w", table, key, err)
		}
	}
	t.written[table] = next
	return nil
}

// Exec runs a statement in the transaction, such as an insert into the
// Hub's history.
func (t *Tx) Exec(query string, args ...any) error {
	_, err := t.tx.Exec(query, args...)
	return err
}

// ErrBroken is the error of every commit after one failed. A store error
// ends the service, and restarting from the last commit is the only way
// back.
var ErrBroken = errors.New("an earlier commit failed: restart the service")
