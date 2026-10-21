package daisy

import (
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ivoras/daisy/internal/assets"
)

// makeSQLiteFile creates a database file with the given statements run in it
func makeSQLiteFile(t *testing.T, fileName string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite3", fileName)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatal(err)
	}
	for _, q := range stmts {
		if _, err = db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestBlockDSNEscapesTheFileName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"plain.db", "we?ird#na%me&mode=rw.db", "with space.db", "100%.db"} {
		// The normal driver can't create a file with a name like that, so it's renamed
		fn := filepath.Join(dir, name)
		makeSQLiteFile(t, filepath.Join(dir, "created.db"), "CREATE TABLE t (a)", "INSERT INTO t VALUES (42)")
		if err := os.Rename(filepath.Join(dir, "created.db"), fn); err != nil {
			t.Fatal(err)
		}
		db, err := dbOpen(fn, true)
		if err != nil {
			t.Fatal(err)
		}
		var a int
		if err = db.QueryRow("SELECT a FROM t").Scan(&a); err != nil || a != 42 {
			t.Errorf("%s: %d, %v", name, a, err)
		}
		// It's read-only, whatever the name says
		if _, err = db.Exec("INSERT INTO t VALUES (1)"); err == nil {
			t.Errorf("%s: could write to a block", name)
		}
		db.Close()
	}
}

func TestBlockConnectionIsHardened(t *testing.T) {
	fn := filepath.Join(t.TempDir(), "b.db")
	makeSQLiteFile(t, fn, "CREATE TABLE t (a)")
	db, err := dbOpen(fn, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for pragma, want := range map[string]int{"trusted_schema": 0, "query_only": 1} {
		var got int
		if err = db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Errorf("PRAGMA %s is %d (%v), want %d", pragma, got, err, want)
		}
	}
	for _, q := range []string{
		"CREATE TABLE x (a)",
		"INSERT INTO t VALUES (1)",
		"ATTACH DATABASE ':memory:' AS other",
		"SELECT zeroblob(300000000)", // More than the limit of the size of a value
	} {
		if _, err = db.Exec(q); err == nil {
			t.Errorf("%s: it must not work on a block", q)
		}
	}
	// The system databases are not affected
	sys, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "sys.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sys.Close()
	if _, err = sys.Exec("CREATE TABLE t (a)"); err != nil {
		t.Errorf("the normal driver is restricted: %v", err)
	}
}

func TestBlockWithAViewAsMetaIsRefused(t *testing.T) {
	dir := t.TempDir()
	keys := `CREATE TABLE _keys (op CHAR NOT NULL, pubkey_hash VARCHAR NOT NULL, pubkey VARCHAR NOT NULL, sigkey_hash VARCHAR NOT NULL, signature VARCHAR NOT NULL, metadata VARCHAR, PRIMARY KEY (pubkey_hash, sigkey_hash))`
	for name, stmts := range map[string][]string{
		"_meta is a view": {
			"CREATE TABLE real_meta (key VARCHAR NOT NULL PRIMARY KEY, value VARCHAR)",
			"CREATE VIEW _meta AS SELECT key, value FROM real_meta",
			keys,
		},
		"_meta is a recursive view": {
			"CREATE VIEW _meta AS WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c) SELECT 'Version' AS key, max(x) AS value FROM c",
			keys,
		},
		"_keys is a view": {
			"CREATE TABLE _meta (key VARCHAR NOT NULL PRIMARY KEY, value VARCHAR)",
			"CREATE TABLE real_keys (op, pubkey_hash, pubkey, sigkey_hash, signature, metadata)",
			"CREATE VIEW _keys AS SELECT * FROM real_keys",
		},
		"_meta is a virtual table": {
			"CREATE VIRTUAL TABLE _meta USING fts3(key, value)",
			keys,
		},
		"there is no _keys": {
			"CREATE TABLE _meta (key VARCHAR NOT NULL PRIMARY KEY, value VARCHAR)",
		},
	} {
		fn := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".db")
		makeSQLiteFile(t, fn, stmts...)
		b, err := OpenBlockFile(fn)
		if err == nil {
			b.Close()
			t.Errorf("%s: the block was opened", name)
			continue
		}
		if _, err = readKeyOps(fn); err == nil {
			t.Errorf("%s: the key ops were read", name)
		}
		if _, err = readBlockTimestamp(fn); err == nil {
			t.Errorf("%s: the timestamp was read", name)
		}
	}
}

func TestCorruptBlockIsRefused(t *testing.T) {
	signer := newTestSigner(t)
	tc := setupTestChain(t, signer)
	good := tc.newBlock(tc.genesis.hash, signer, "to be damaged")
	data, err := os.ReadFile(good.file)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 3*4096 {
		t.Fatalf("the block file is just %d bytes", len(data))
	}

	// Not a database at all
	notDb := filepath.Join(t.TempDir(), "garbage.db")
	if err = os.WriteFile(notDb, []byte(strings.Repeat("this is not a SQLite database\n", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := OpenBlockFile(notDb); err == nil {
		b.Close()
		t.Error("garbage was opened as a block")
	}

	// Pages which are damaged, one at a time, after the first one (which has the schema)
	refused := 0
	for page := 1; page < len(data)/4096; page++ {
		damaged := append([]byte(nil), data...)
		for i := page * 4096; i < (page+1)*4096; i++ {
			damaged[i] = 0xAA
		}
		fn := filepath.Join(t.TempDir(), "damaged.db")
		if err = os.WriteFile(fn, damaged, 0o600); err != nil {
			t.Fatal(err)
		}
		b, err := OpenBlockFile(fn)
		if err != nil {
			refused++
			continue
		}
		if err = b.QuickCheck(); err != nil {
			refused++
		}
		b.Close()
	}
	if refused != len(data)/4096-1 {
		t.Errorf("only %d of %d damaged blocks were refused", refused, len(data)/4096-1)
	}

	// The whole path: a damaged block is not accepted, the intact one is
	damaged := append([]byte(nil), data...)
	for i := len(data) - 4096; i < len(data); i++ {
		damaged[i] = 0xAA
	}
	bad := good
	bad.file = filepath.Join(t.TempDir(), "bad.db")
	if err = os.WriteFile(bad.file, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	tc.mustRefuse(bad)
	tc.mustAccept(good, blockNewTip)
	tc.checkConsistent()
}

func TestBlockWithTooManyKeyRecordsIsRefused(t *testing.T) {
	dir := t.TempDir()
	target := newTestSigner(t)
	for _, n := range []int{maxBlockKeyOps, maxBlockKeyOps + 1, 100000} {
		fn := filepath.Join(dir, "keys.db")
		os.Remove(fn)
		makeSQLiteFile(t, fn,
			"CREATE TABLE _meta (key VARCHAR NOT NULL PRIMARY KEY, value VARCHAR)",
			"INSERT INTO _meta VALUES ('Version', '1'), ('PreviousBlockHash', 'aa'), ('CreatorPublicKey', 'bb'), ('PreviousBlockHashSignature', 'cc')",
			"CREATE TABLE _keys (op CHAR NOT NULL, pubkey_hash VARCHAR NOT NULL, pubkey VARCHAR NOT NULL, sigkey_hash VARCHAR NOT NULL, signature VARCHAR NOT NULL, metadata VARCHAR, PRIMARY KEY (pubkey_hash, sigkey_hash))",
			"WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < "+strconv.Itoa(n)+") INSERT INTO _keys SELECT 'A', '"+target.hash+"', '"+hex.EncodeToString(target.pub)+"', 's' || i, 'aa', NULL FROM c",
		)
		b, err := OpenBlockFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		_, err = b.dbGetKeyOps()
		b.Close()
		tooMany := err != nil && strings.Contains(err.Error(), "more than")
		if tooMany != (n > maxBlockKeyOps) {
			t.Errorf("%d records: %v", n, err)
		}
		if n <= maxBlockKeyOps && err != nil {
			t.Errorf("%d records are fine: %v", n, err)
		}
	}
}

// Blocks are arbitrary databases: the objects in them which are not _meta and _keys are not the business of Daisy
func TestBlockWithUserViewsAndTriggersIsAccepted(t *testing.T) {
	signer := newTestSigner(t)
	tc := setupTestChain(t, signer)
	tc.extraSQL = []string{
		"CREATE TABLE audit (v TEXT)",
		"CREATE INDEX payload_v ON payload(v)",
		"CREATE VIEW payload_view AS SELECT upper(v) AS u FROM payload",
		"CREATE VIEW numbers AS WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 10) SELECT x FROM c",
		"CREATE TRIGGER payload_audit AFTER INSERT ON payload BEGIN INSERT INTO audit VALUES (new.v); END",
		"INSERT INTO payload VALUES ('after the trigger')",
		"CREATE TABLE checked (n INTEGER CHECK (n > 0), g INTEGER GENERATED ALWAYS AS (n * 2) VIRTUAL)",
		"INSERT INTO checked (n) VALUES (3)",
	}
	b1 := tc.newBlock(tc.genesis.hash, signer, "user objects")
	tc.mustAccept(b1, blockNewTip)
	tc.checkConsistent()

	// The views can be queried in the stored block (as "daisy query" does)
	db, err := dbOpen(blockchainGetFilename(1), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sum, audited int
	if err = db.QueryRow("SELECT sum(x) FROM numbers").Scan(&sum); err != nil || sum != 55 {
		t.Errorf("the view of the block: %d, %v", sum, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM payload_view WHERE u = 'USER OBJECTS'").Scan(&audited); err != nil || audited != 1 {
		t.Errorf("the view of the block: %d, %v", audited, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM audit").Scan(&audited); err != nil || audited != 1 {
		t.Errorf("the table of the block: %d, %v", audited, err)
	}
}

// The embedded genesis block must pass the same checks as any other block
func TestGenesisBlockPassesTheChecks(t *testing.T) {
	fn := filepath.Join(t.TempDir(), "genesis.db")
	if err := os.WriteFile(fn, assets.GenesisDB, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBlockFile(fn)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.QuickCheck(); err != nil {
		t.Error(err)
	}
	if _, err = b.dbGetKeyOps(); err != nil {
		t.Error(err)
	}
}
