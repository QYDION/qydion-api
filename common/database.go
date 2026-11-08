package common

import "strings"

type DatabaseType string

const (
	DatabaseTypeMySQL      DatabaseType = "mysql"
	DatabaseTypeSQLite     DatabaseType = "sqlite"
	DatabaseTypePostgreSQL DatabaseType = "postgres"
	DatabaseTypeClickHouse DatabaseType = "clickhouse"
)

var mainDatabaseType = DatabaseTypeSQLite
var logDatabaseType = DatabaseTypeSQLite

func MainDatabaseType() DatabaseType {
	return mainDatabaseType
}

func LogDatabaseType() DatabaseType {
	return logDatabaseType
}

func SetMainDatabaseType(databaseType DatabaseType) {
	mainDatabaseType = databaseType
}

func SetLogDatabaseType(databaseType DatabaseType) {
	logDatabaseType = databaseType
}

func SetDatabaseTypes(mainType DatabaseType, logType DatabaseType) {
	mainDatabaseType = mainType
	logDatabaseType = logType
}

func UsingMainDatabase(databaseType DatabaseType) bool {
	return mainDatabaseType == databaseType
}

func UsingLogDatabase(databaseType DatabaseType) bool {
	return logDatabaseType == databaseType
}

// SQLitePath is the DSN for the default SQLite database. It uses WAL journal
// mode so readers are never blocked by the single writer, plus a 30s busy
// timeout for writers to queue.
//
// Two details are non-obvious and both are required for concurrent correctness:
//
//  1. The busy timeout must be passed as a `_pragma=busy_timeout(30000)` DSN
//     parameter. The pure-Go driver (modernc.org/sqlite, used through
//     github.com/glebarez/sqlite) silently ignores the plain `_busy_timeout=`
//     form, so without this the effective timeout stays at SQLite's 5s default
//     and concurrent writes surface as "database is locked" (see #6805).
//
//  2. `_txlock=immediate` (BEGIN IMMEDIATE) must be enabled. Without it, a
//     transaction that first SELECTs (establishing a read snapshot) and then
//     writes can hit SQLITE_BUSY_SNAPSHOT when another connection commits in
//     between; the busy handler does not cover that case, so the write fails
//     instantly no matter the timeout. BEGIN IMMEDIATE takes the write lock up
//     front, so writers serialize through the busy timeout instead of dying on
//     a stale snapshot. Autocommit SELECTs stay concurrent because WAL keeps
//     readers unlocked.
var SQLitePath = "one-api.db?" + sqliteConcurrencyParams

// sqliteConcurrencyParams are the connection parameters behind every claim
// above. They belong to the DSN, not to the file name.
const sqliteConcurrencyParams = "_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_txlock=immediate"

// WithSQLiteConcurrencyParams turns a bare database file path into a DSN that
// still carries the concurrency parameters. SQLITE_PATH has historically been
// set to nothing but a file name, and assigning it over the whole DSN used to
// drop the pragmas with it — leaving the production database in DELETE journal
// mode with the driver's 5s default timeout.
//
// A value that already carries a query string is left untouched: then the
// operator has stated the connection parameters and guessing over them would
// override a deliberate choice.
func WithSQLiteConcurrencyParams(dsn string) string {
	if strings.Contains(dsn, "?") {
		return dsn
	}
	// A shared-cache in-memory database has no journal and no on-disk writer to
	// wait for, so the file-oriented parameters do not apply.
	if dsn == ":memory:" || strings.HasPrefix(dsn, "file:") {
		return dsn
	}
	return dsn + "?" + sqliteConcurrencyParams
}
