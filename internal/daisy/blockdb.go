package daisy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

/*
 * Opening block files.
 *
 * A block is an arbitrary SQLite database file, and most of them come from other nodes, so SQLite is parsing and
 * running things from a file which an attacker made. That is what the hardening here is for, and it applies to every
 * read-only open (dbOpen with readOnly set), which is how all the blocks are opened. The system databases and the
 * files which this node writes (a block being created by signimportblock or newchain) do not use it.
 *
 *  - The file is opened with mode=ro, and the connection is query_only. Nothing here can change a block.
 *  - trusted_schema is OFF: the views, triggers, generated columns and CHECK constraints in the file cannot call SQL
 *    functions which are not marked as safe for that. (Daisy blocks are arbitrary databases, so views and triggers of
 *    the user's tables are fine and nothing rejects them; they are only never trusted.)
 *  - The limits of the SQL engine are lowered (see blockLimits): the size of a value, of a SQL statement (which
 *    includes the schema of the file), the depth of an expression, the size of a compound select, the number of VDBE
 *    operations in one statement, the depth of triggers; and no other database can be attached. The page cache is small.
 *  - The data which the consensus rules look at is read from the tables _meta and _keys only, which must be real tables
 *    of the file and not views or virtual tables (dbCheckBlockTables), because a view could be made to run an expensive
 *    query (recursive CTEs) whenever it is read. The reads have a time limit (blockQueryTimeout), and the number of
 *    _keys records is limited (maxBlockKeyOps).
 *  - Before the first block of a peer is trusted with anything, PRAGMA quick_check makes sure that the file is not
 *    corrupt (dbQuickCheck).
 *
 * SQLITE_DBCONFIG_DEFENSIVE is not set: go-sqlite3 doesn't let the program reach sqlite3_db_config(), and it only
 * protects a database which is being written, which a read-only connection doesn't do.
 */

const (
	// The name of the database/sql driver which opens block files
	blockDriverName = "sqlite3_block"

	// The time limit for reading the metadata and key ops of a block
	blockQueryTimeout = 20 * time.Second

	// The time limit for PRAGMA quick_check, which reads the entire file (which can be up to p2pMaxBlockSize)
	blockCheckTimeout = 5 * time.Minute

	// A block can't have more key op records than this
	maxBlockKeyOps = 1000

	// The size of the page cache of a block connection, in KiB
	blockCacheKiB = 4096
)

// blockLimits are the run-time limits of the connections which read blocks (see sqlite3_limit()). They are well
// below the defaults of SQLite, and well above what normal databases need.
var blockLimits = []struct{ id, value int }{
	{sqlite3.SQLITE_LIMIT_LENGTH, 256 << 20},         // the biggest string or blob (the default is 1,000,000,000)
	{sqlite3.SQLITE_LIMIT_SQL_LENGTH, 4 << 20},       // a statement, including the CREATE statements in the schema (1,000,000,000)
	{sqlite3.SQLITE_LIMIT_EXPR_DEPTH, 200},           // (1000)
	{sqlite3.SQLITE_LIMIT_COMPOUND_SELECT, 100},      // (500)
	{sqlite3.SQLITE_LIMIT_VDBE_OP, 1000000},          // (250,000,000)
	{sqlite3.SQLITE_LIMIT_FUNCTION_ARG, 32},          // (127)
	{sqlite3.SQLITE_LIMIT_ATTACHED, 0},               // no other database can be attached (10)
	{sqlite3.SQLITE_LIMIT_LIKE_PATTERN_LENGTH, 1000}, // (50,000)
	{sqlite3.SQLITE_LIMIT_TRIGGER_DEPTH, 10},         // (1000)
}

func init() {
	sql.Register(blockDriverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			for _, pragma := range []string{
				"PRAGMA trusted_schema=OFF",
				"PRAGMA query_only=ON",
				fmt.Sprintf("PRAGMA cache_size=-%d", blockCacheKiB),
			} {
				if _, err := conn.Exec(pragma, nil); err != nil {
					return fmt.Errorf("%s: %w", pragma, err)
				}
			}
			for _, l := range blockLimits {
				conn.SetLimit(l.id, l.value)
			}
			return nil
		},
	})
}

// blockDSN returns the data source name which opens the given file read-only. The name of the file is escaped, so
// that characters like ? and # and % in it can't be taken for the parts of a URI.
func blockDSN(fileName string) string {
	if abs, err := filepath.Abs(fileName); err == nil {
		fileName = abs
	}
	fileName = filepath.ToSlash(fileName)
	if len(fileName) == 0 || fileName[0] != '/' {
		fileName = "/" + fileName // A Windows drive letter
	}
	u := url.URL{Scheme: "file", Path: fileName, RawQuery: url.Values{"mode": {"ro"}}.Encode()}
	return u.String()
}

// dbCheckBlockTables checks that _meta and _keys are real tables in the database of a block: not views or virtual
// tables, which could run any query whenever they are read.
func dbCheckBlockTables(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), blockQueryTimeout)
	defer cancel()
	for _, name := range []string{"_meta", "_keys"} {
		var kind string
		err := db.QueryRowContext(ctx, "SELECT type FROM pragma_table_list WHERE schema='main' AND name=?", name).Scan(&kind)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("The block has no table %s", name)
		}
		if err != nil {
			return fmt.Errorf("Cannot check the table %s of the block: %w", name, err)
		}
		if kind != "table" {
			return fmt.Errorf("%s in the block is a %s, and it must be a table", name, kind)
		}
	}
	return nil
}

// dbQuickCheck runs PRAGMA quick_check on the database of a block, and fails if it finds a problem
func dbQuickCheck(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), blockCheckTimeout)
	defer cancel()
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&result); err != nil {
		return fmt.Errorf("The block database check has failed: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("The block database is corrupt: %s", result)
	}
	return nil
}
