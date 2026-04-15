package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/cynkra/blockyard/internal/config"
)

// ---------------------------------------------------------------------------
// Schema dump helpers
// ---------------------------------------------------------------------------

func dumpSQLiteSchema(t *testing.T, db *DB) string {
	t.Helper()
	rows, err := db.Query(
		`SELECT sql FROM sqlite_master
		 WHERE sql IS NOT NULL
		   AND name != 'schema_migrations'
		 ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var stmts []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		stmts = append(stmts, s)
	}
	return strings.Join(stmts, "\n")
}

func dumpPostgresSchema(t *testing.T, db *DB) string {
	t.Helper()

	// Tables and columns
	rows, err := db.Query(`
		SELECT table_name, column_name, data_type,
		       column_default, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name != 'schema_migrations'
		ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var tbl, col, dtype, nullable string
		var dflt *string
		if err := rows.Scan(&tbl, &col, &dtype, &dflt, &nullable); err != nil {
			t.Fatal(err)
		}
		d := "NULL"
		if dflt != nil {
			d = *dflt
		}
		lines = append(lines, fmt.Sprintf("%s.%s %s default=%s nullable=%s",
			tbl, col, dtype, d, nullable))
	}

	// Indexes
	idxRows, err := db.Query(`
		SELECT tablename, indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = 'public'
		  AND tablename != 'schema_migrations'
		ORDER BY tablename, indexname`)
	if err != nil {
		t.Fatal(err)
	}
	defer idxRows.Close()

	for idxRows.Next() {
		var tbl, name, def string
		if err := idxRows.Scan(&tbl, &name, &def); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("INDEX %s: %s", name, def))
	}

	// CHECK constraints (exclude system-generated NOT NULL constraints whose
	// names contain OIDs that change across drop/create cycles)
	chkRows, err := db.Query(`
		SELECT tc.table_name, cc.constraint_name, cc.check_clause
		FROM information_schema.check_constraints cc
		JOIN information_schema.table_constraints tc
		    ON cc.constraint_name = tc.constraint_name
		   AND cc.constraint_schema = tc.constraint_schema
		WHERE tc.table_schema = 'public'
		  AND tc.table_name != 'schema_migrations'
		  AND cc.constraint_name NOT LIKE '%_not_null'
		ORDER BY tc.table_name, cc.constraint_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer chkRows.Close()

	for chkRows.Next() {
		var tbl, name, clause string
		if err := chkRows.Scan(&tbl, &name, &clause); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("CHECK %s.%s: %s", tbl, name, clause))
	}

	// Foreign key constraints — use pg_catalog for deterministic column
	// ordering (information_schema.constraint_column_usage cross-joins
	// on composite FKs).
	fkRows, err := db.Query(`
		SELECT c.conrelid::regclass AS table_name,
		       c.conname,
		       a1.attname AS column_name,
		       c.confrelid::regclass AS ref_table,
		       a2.attname AS ref_column
		FROM pg_constraint c
		JOIN LATERAL unnest(c.conkey, c.confkey)
		     WITH ORDINALITY AS u(attnum, refattnum, ord) ON true
		JOIN pg_attribute a1
		     ON a1.attrelid = c.conrelid AND a1.attnum = u.attnum
		JOIN pg_attribute a2
		     ON a2.attrelid = c.confrelid AND a2.attnum = u.refattnum
		WHERE c.contype = 'f'
		  AND c.connamespace = 'public'::regnamespace
		ORDER BY c.conrelid::regclass::text, c.conname, u.ord`)
	if err != nil {
		t.Fatal(err)
	}
	defer fkRows.Close()

	for fkRows.Next() {
		var tbl, name, col, refTbl, refCol string
		if err := fkRows.Scan(&tbl, &name, &col, &refTbl, &refCol); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("FK %s.%s: %s -> %s.%s",
			tbl, name, col, refTbl, refCol))
	}

	// Functions (excludes internal/system functions)
	fnRows, err := db.Query(`
		SELECT p.proname, pg_get_functiondef(p.oid)
		FROM pg_proc p
		JOIN pg_namespace n ON p.pronamespace = n.oid
		WHERE n.nspname = 'public'
		ORDER BY p.proname`)
	if err != nil {
		t.Fatal(err)
	}
	defer fnRows.Close()

	for fnRows.Next() {
		var name, def string
		if err := fnRows.Scan(&name, &def); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("FUNC %s: %s", name, def))
	}

	// Triggers
	trgRows, err := db.Query(`
		SELECT tgname, pg_get_triggerdef(t.oid)
		FROM pg_trigger t
		JOIN pg_class c ON t.tgrelid = c.oid
		JOIN pg_namespace n ON c.relnamespace = n.oid
		WHERE n.nspname = 'public'
		  AND NOT t.tgisinternal
		ORDER BY c.relname, t.tgname`)
	if err != nil {
		t.Fatal(err)
	}
	defer trgRows.Close()

	for trgRows.Next() {
		var name, def string
		if err := trgRows.Scan(&name, &def); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("TRIGGER %s: %s", name, def))
	}

	// RLS policies
	polRows, err := db.Query(`
		SELECT pol.polname, c.relname, pg_get_expr(pol.polqual, pol.polrelid) AS using_expr,
		       pg_get_expr(pol.polwithcheck, pol.polrelid) AS check_expr
		FROM pg_policy pol
		JOIN pg_class c ON pol.polrelid = c.oid
		JOIN pg_namespace n ON c.relnamespace = n.oid
		WHERE n.nspname = 'public'
		ORDER BY c.relname, pol.polname`)
	if err != nil {
		t.Fatal(err)
	}
	defer polRows.Close()

	for polRows.Next() {
		var name, tbl string
		var usingExpr, checkExpr *string
		if err := polRows.Scan(&name, &tbl, &usingExpr, &checkExpr); err != nil {
			t.Fatal(err)
		}
		u, c := "NULL", "NULL"
		if usingExpr != nil {
			u = *usingExpr
		}
		if checkExpr != nil {
			c = *checkExpr
		}
		lines = append(lines, fmt.Sprintf("POLICY %s.%s: USING(%s) CHECK(%s)",
			tbl, name, u, c))
	}

	return strings.Join(lines, "\n")
}

func dumpSchema(t *testing.T, db *DB) string {
	t.Helper()
	switch db.dialect {
	case DialectSQLite:
		return dumpSQLiteSchema(t, db)
	case DialectPostgres:
		return dumpPostgresSchema(t, db)
	default:
		t.Fatalf("unknown dialect: %d", db.dialect)
		return ""
	}
}

// ---------------------------------------------------------------------------
// File convention check
// ---------------------------------------------------------------------------

func TestMigrationConventions(t *testing.T) {
	released := parseReleased(t)

	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var fsys fs.FS
			var err error
			switch dialect {
			case "sqlite":
				fsys, err = fs.Sub(sqliteMigrations, "migrations/sqlite")
			case "postgres":
				fsys, err = fs.Sub(postgresMigrations, "migrations/postgres")
			}
			if err != nil {
				t.Fatal(err)
			}
			checkConventions(t, dialect, fsys, released)
		})
	}

	// Cross-dialect: matching migration numbers
	sqliteNums := migrationNumbers(t, sqliteMigrations, "migrations/sqlite")
	pgNums := migrationNumbers(t, postgresMigrations, "migrations/postgres")
	if !reflect.DeepEqual(sqliteNums, pgNums) {
		t.Errorf("migration numbers differ: sqlite=%v postgres=%v",
			sqliteNums, pgNums)
	}
}

// findConventionViolations checks migration files for convention violations
// and returns a list of human-readable descriptions. Separated from
// checkConventions so negative tests can inspect violations without
// propagating subtest failures.
func findConventionViolations(fsys fs.FS, released map[int]bool) []string {
	var violations []string
	addf := func(format string, args ...any) {
		violations = append(violations, fmt.Sprintf(format, args...))
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return []string{fmt.Sprintf("read dir: %v", err)}
	}

	ups := map[int]string{}
	downs := map[int]string{}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}

		// Parse NNN_description.{up,down}.sql
		parts := strings.SplitN(name, "_", 2)
		num, err := strconv.Atoi(parts[0])
		if err != nil {
			addf("%s: migration number is not an integer: %q", name, parts[0])
			continue
		}

		switch {
		case strings.HasSuffix(name, ".up.sql"):
			ups[num] = name
		case strings.HasSuffix(name, ".down.sql"):
			downs[num] = name
		default:
			addf("%s: unexpected suffix (want .up.sql or .down.sql)", name)
		}
	}

	// Every up has a matching down and vice versa
	for num, name := range ups {
		if _, ok := downs[num]; !ok {
			addf("%s: missing matching .down.sql", name)
		}
	}
	for num, name := range downs {
		if _, ok := ups[num]; !ok {
			addf("%s: missing matching .up.sql", name)
		}
	}

	// Sequential numbering with no gaps
	var nums []int
	for num := range ups {
		nums = append(nums, num)
	}
	sort.Ints(nums)
	for i, num := range nums {
		expected := i + 1
		if num != expected {
			addf("gap in migration numbering: expected %03d, got %03d", expected, num)
		}
	}

	// No empty files
	for _, name := range ups {
		if data, err := fs.ReadFile(fsys, name); err == nil {
			if strings.TrimSpace(string(data)) == "" {
				addf("%s: migration file is empty", name)
			}
		}
	}
	for _, name := range downs {
		if data, err := fs.ReadFile(fsys, name); err == nil {
			if strings.TrimSpace(string(data)) == "" {
				addf("%s: migration file is empty", name)
			}
		}
	}

	// Phase tags (up files only)
	phases := map[int]string{}
	for num, name := range ups {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			continue
		}
		first, _, _ := strings.Cut(string(data), "\n")
		first = strings.TrimSpace(first)
		switch first {
		case "-- phase: expand":
			phases[num] = "expand"
		case "-- phase: contract":
			phases[num] = "contract"
		default:
			addf("%s: first line must be '-- phase: expand' or '-- phase: contract', got %q",
				name, first)
		}
	}

	// Contract referential integrity
	for num, phase := range phases {
		if phase != "contract" {
			continue
		}
		refs := parseContractRefs(fsys, ups[num])
		if len(refs) == 0 {
			addf("%s: contract migration missing -- contracts: NNN", ups[num])
			continue
		}
		for _, ref := range refs {
			if ref >= num {
				addf("%s: contracts reference %03d must be lower than %03d",
					ups[num], ref, num)
			}
			if _, ok := ups[ref]; !ok {
				addf("%s: contracts reference %03d does not exist",
					ups[num], ref)
			} else if phases[ref] != "expand" {
				addf("%s: contracts reference %03d is %q, not expand",
					ups[num], ref, phases[ref])
			}
			if !released[ref] {
				addf("%s: contracts reference %03d has not been released "+
					"(not in released.txt)", ups[num], ref)
			}
		}
	}

	return violations
}

// checkConventions runs findConventionViolations and reports each violation
// via t.Errorf. Used by TestMigrationConventions for the real migration files.
func checkConventions(t *testing.T, dialect string, fsys fs.FS, released map[int]bool) {
	t.Helper()
	for _, v := range findConventionViolations(fsys, released) {
		t.Errorf("[%s] %s", dialect, v)
	}
}

// parseContractRefs parses -- contracts: NNN[, NNN...] from a migration file.
func parseContractRefs(fsys fs.FS, name string) []int {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil
	}
	var refs []int
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-- contracts:") {
			continue
		}
		val := strings.TrimPrefix(line, "-- contracts:")
		for _, s := range strings.Split(val, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			num, err := strconv.Atoi(s)
			if err != nil {
				continue
			}
			refs = append(refs, num)
		}
	}
	return refs
}

// parseReleased reads migrations/released.txt and returns the set of
// migration numbers that have shipped in a release.
func parseReleased(t *testing.T) map[int]bool {
	t.Helper()
	data, err := releasedFile.ReadFile("migrations/released.txt")
	if err != nil {
		t.Fatal(err)
	}
	released := map[int]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Errorf("released.txt: malformed line: %q", line)
			continue
		}
		num, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Errorf("released.txt: invalid migration number: %q", fields[0])
			continue
		}
		released[num] = true
	}
	return released
}

func migrationNumbers(t *testing.T, embedFS embed.FS, dir string) []int {
	t.Helper()
	fsys, err := fs.Sub(embedFS, dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, e := range entries {
		parts := strings.SplitN(e.Name(), "_", 2)
		num, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		seen[num] = true
	}
	var nums []int
	for num := range seen {
		nums = append(nums, num)
	}
	sort.Ints(nums)
	return nums
}

// ---------------------------------------------------------------------------
// Convention check negative tests — verify that violations are caught
// ---------------------------------------------------------------------------

func TestConventionCheckRejectsViolations(t *testing.T) {
	noReleases := map[int]bool{}

	cases := []struct {
		name     string
		fsys     fstest.MapFS
		released map[int]bool
		wantFail bool
	}{
		{
			name: "valid_baseline",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("-- phase: expand\nCREATE TABLE t (id INT);\n")},
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
			},
			released: noReleases,
		},
		{
			name: "missing_down",
			fsys: fstest.MapFS{
				"001_initial.up.sql": {Data: []byte("-- phase: expand\nCREATE TABLE t (id INT);\n")},
			},
			released: noReleases,
			wantFail: true,
		},
		{
			name: "missing_up",
			fsys: fstest.MapFS{
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
			},
			released: noReleases,
			wantFail: true,
		},
		{
			name: "numbering_gap",
			fsys: fstest.MapFS{
				"001_first.up.sql":   {Data: []byte("-- phase: expand\nCREATE TABLE a (id INT);\n")},
				"001_first.down.sql": {Data: []byte("DROP TABLE a;\n")},
				"003_third.up.sql":   {Data: []byte("-- phase: expand\nCREATE TABLE b (id INT);\n")},
				"003_third.down.sql": {Data: []byte("DROP TABLE b;\n")},
			},
			released: noReleases,
			wantFail: true,
		},
		{
			name: "bad_phase_tag",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("-- phase: unknown\nCREATE TABLE t (id INT);\n")},
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
			},
			released: noReleases,
			wantFail: true,
		},
		{
			name: "no_phase_tag",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("CREATE TABLE t (id INT);\n")},
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
			},
			released: noReleases,
			wantFail: true,
		},
		{
			name: "empty_up",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("")},
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
			},
			released: noReleases,
			wantFail: true,
		},
		{
			name: "empty_down",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("-- phase: expand\nCREATE TABLE t (id INT);\n")},
				"001_initial.down.sql": {Data: []byte("")},
			},
			released: noReleases,
			wantFail: true,
		},
		{
			name: "contract_no_ref",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("-- phase: expand\nCREATE TABLE t (id INT);\n")},
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
				"002_cleanup.up.sql":   {Data: []byte("-- phase: contract\nDROP COLUMN x;\n")},
				"002_cleanup.down.sql": {Data: []byte("ALTER TABLE t ADD COLUMN x INT;\n")},
			},
			released: map[int]bool{1: true},
			wantFail: true,
		},
		{
			name: "contract_ref_higher",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("-- phase: expand\nCREATE TABLE t (id INT);\n")},
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
				"002_cleanup.up.sql":   {Data: []byte("-- phase: contract\n-- contracts: 003\nDROP COLUMN x;\n")},
				"002_cleanup.down.sql": {Data: []byte("ALTER TABLE t ADD COLUMN x INT;\n")},
			},
			released: map[int]bool{1: true, 3: true},
			wantFail: true,
		},
		{
			name: "contract_ref_nonexistent",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("-- phase: expand\nCREATE TABLE t (id INT);\n")},
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
				"002_cleanup.up.sql":   {Data: []byte("-- phase: contract\n-- contracts: 099\nDROP COLUMN x;\n")},
				"002_cleanup.down.sql": {Data: []byte("ALTER TABLE t ADD COLUMN x INT;\n")},
			},
			released: map[int]bool{99: true},
			wantFail: true,
		},
		{
			name: "contract_ref_contract",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("-- phase: expand\nCREATE TABLE t (id INT);\n")},
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
				"002_cleanup.up.sql":   {Data: []byte("-- phase: contract\n-- contracts: 001\nDROP COLUMN x;\n")},
				"002_cleanup.down.sql": {Data: []byte("ALTER TABLE t ADD COLUMN x INT;\n")},
				"003_more.up.sql":      {Data: []byte("-- phase: contract\n-- contracts: 002\nDROP COLUMN y;\n")},
				"003_more.down.sql":    {Data: []byte("ALTER TABLE t ADD COLUMN y INT;\n")},
			},
			released: map[int]bool{1: true, 2: true},
			wantFail: true,
		},
		{
			name: "contract_ref_unreleased",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("-- phase: expand\nCREATE TABLE t (id INT);\n")},
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
				"002_add_col.up.sql":   {Data: []byte("-- phase: expand\nALTER TABLE t ADD COLUMN x INT;\n")},
				"002_add_col.down.sql": {Data: []byte("ALTER TABLE t DROP COLUMN x;\n")},
				"003_cleanup.up.sql":   {Data: []byte("-- phase: contract\n-- contracts: 002\nDROP COLUMN y;\n")},
				"003_cleanup.down.sql": {Data: []byte("ALTER TABLE t ADD COLUMN y INT;\n")},
			},
			released: map[int]bool{1: true}, // 002 not released
			wantFail: true,
		},
		{
			name: "valid_contract",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("-- phase: expand\nCREATE TABLE t (id INT);\n")},
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
				"002_add_col.up.sql":   {Data: []byte("-- phase: expand\nALTER TABLE t ADD COLUMN x INT;\n")},
				"002_add_col.down.sql": {Data: []byte("ALTER TABLE t DROP COLUMN x;\n")},
				"003_cleanup.up.sql":   {Data: []byte("-- phase: contract\n-- contracts: 002\nDROP COLUMN y;\n")},
				"003_cleanup.down.sql": {Data: []byte("ALTER TABLE t ADD COLUMN y INT;\n")},
			},
			released: map[int]bool{1: true, 2: true},
		},
		{
			name: "valid_multi_ref_contract",
			fsys: fstest.MapFS{
				"001_initial.up.sql":   {Data: []byte("-- phase: expand\nCREATE TABLE t (id INT);\n")},
				"001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
				"002_add_x.up.sql":     {Data: []byte("-- phase: expand\nALTER TABLE t ADD COLUMN x INT;\n")},
				"002_add_x.down.sql":   {Data: []byte("ALTER TABLE t DROP COLUMN x;\n")},
				"003_add_y.up.sql":     {Data: []byte("-- phase: expand\nALTER TABLE t ADD COLUMN y INT;\n")},
				"003_add_y.down.sql":   {Data: []byte("ALTER TABLE t DROP COLUMN y;\n")},
				"004_cleanup.up.sql":   {Data: []byte("-- phase: contract\n-- contracts: 002, 003\nDROP stuff;\n")},
				"004_cleanup.down.sql": {Data: []byte("ADD stuff;\n")},
			},
			released: map[int]bool{1: true, 2: true, 3: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			violations := findConventionViolations(tc.fsys, tc.released)
			if tc.wantFail && len(violations) == 0 {
				t.Error("expected convention check to find violations, but it passed")
			}
			if !tc.wantFail && len(violations) > 0 {
				t.Errorf("expected no violations, got: %v", violations)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Up-down-up roundtrip test
// ---------------------------------------------------------------------------

func TestMigrateRoundtrip(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		db := openRawSQLite(t)
		roundtrip(t, db)
	})
	t.Run("postgres", func(t *testing.T) {
		db := openRawPostgres(t)
		if db == nil {
			return // skipped
		}
		roundtrip(t, db)
	})
}

func roundtrip(t *testing.T, db *DB) {
	t.Helper()

	m, cleanup, err := db.newMigrator()
	if err != nil {
		t.Fatal(err)
	}

	// Up
	if err := m.Up(); err != nil {
		cleanup()
		t.Fatalf("initial up: %v", err)
	}
	cleanup()
	schemaAfterUp := dumpSchema(t, db)

	// Need a fresh migrator — golang-migrate is stateful
	m, cleanup, err = db.newMigrator()
	if err != nil {
		t.Fatal(err)
	}

	// Down
	if err := m.Down(); err != nil {
		cleanup()
		t.Fatalf("down: %v", err)
	}
	cleanup()

	m, cleanup, err = db.newMigrator()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Up again
	if err := m.Up(); err != nil {
		t.Fatalf("second up: %v", err)
	}
	schemaAfterRoundtrip := dumpSchema(t, db)

	if schemaAfterUp != schemaAfterRoundtrip {
		t.Errorf("schema differs after up-down-up roundtrip:\n--- after first up ---\n%s\n--- after roundtrip ---\n%s",
			schemaAfterUp, schemaAfterRoundtrip)
	}
}

// ---------------------------------------------------------------------------
// Phase 3-5: MigrationVersion / MigrateDown / CheckDownMigrationSafety
// ---------------------------------------------------------------------------

func TestMigrationVersion(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: dir + "/v.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ver, dirty, err := db.MigrationVersion()
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Error("expected clean state")
	}
	if ver == 0 {
		t.Error("expected non-zero version after Open")
	}
}

func TestMigrateDownNoop(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: dir + "/d.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ver, _, _ := db.MigrationVersion()
	if err := db.MigrateDown(ver); err != nil {
		t.Errorf("MigrateDown to current version should noop: %v", err)
	}
}

func TestCheckDownMigrationSafetyOK(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: dir + "/s.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Range that includes real migration 1 — no irreversible marker.
	if err := db.CheckDownMigrationSafety(0, 1); err != nil {
		t.Errorf("expected safe: %v", err)
	}
	// Nonexistent range — empty files are safe.
	if err := db.CheckDownMigrationSafety(100, 105); err != nil {
		t.Errorf("expected safe for missing range: %v", err)
	}
}

func TestReadDownMigrationMissing(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: dir + "/r.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if c := db.readDownMigration(999); c != "" {
		t.Error("expected empty for nonexistent migration")
	}
	// Version 1 exists — should return something (or empty if no down file).
	_ = db.readDownMigration(1)
}

func openRawSQLite(t *testing.T) *DB {
	t.Helper()
	f, err := os.CreateTemp("", "blockyard-roundtrip-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	db, err := sqlx.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	return &DB{DB: db, dialect: DialectSQLite}
}

func openRawPostgres(t *testing.T) *DB {
	t.Helper()
	if pgBaseURL == "" {
		t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set")
		return nil
	}

	dbName := "test_rt_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12]
	admin, err := sql.Open("pgx", pgBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()

	testURL := replaceDBName(pgBaseURL, dbName)
	rawDB, err := sqlx.Open("pgx", testURL)
	if err != nil {
		t.Fatal(err)
	}
	rawDB.SetMaxOpenConns(5)

	t.Cleanup(func() {
		rawDB.Close()
		cleanup, _ := sql.Open("pgx", pgBaseURL)
		cleanup.Exec("DROP DATABASE IF EXISTS " + dbName)
		cleanup.Close()
	})

	return &DB{DB: rawDB, dialect: DialectPostgres, connURL: testURL}
}
