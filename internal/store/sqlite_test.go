package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixture returns the path to a testdata database.
func fixture(name string) string {
	return filepath.Join("testdata", name)
}

// openFixture opens a testdata database, failing the test on error.
func openFixture(t *testing.T, name string) *DB {
	t.Helper()
	db, err := Open(fixture(name))
	if err != nil {
		t.Fatalf("Open(%s): %v", name, err)
	}
	return db
}

func TestOpenValidDatabase(t *testing.T) {
	db := openFixture(t, "kirocli.sqlite3")
	if db.PageSize() != 4096 {
		t.Errorf("PageSize() = %d, want 4096", db.PageSize())
	}

	tables, err := db.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	want := map[string]bool{"auth_kv": true, "state": true}
	for _, name := range tables {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Errorf("Tables() = %v, missing %v", tables, want)
	}
}

func TestOpenRejectsMalformedFiles(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		wantText string
	}{
		{"not a database", "notsqlite.bin", "does not start with the SQLite signature"},
		{"zero length", "zerolength.sqlite3", "too short"},
		{"header only", "truncated.sqlite3", "too short"},
		{"invalid page size", "badpagesize.sqlite3", "not a power of two"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(fixture(tc.file))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantText)
			}
		})
	}
}

func TestOpenMissingFile(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "absent.sqlite3"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if !strings.Contains(err.Error(), "could not read") {
		t.Errorf("error = %q", err)
	}
}

func TestOpenRefusesWALFromTheHeader(t *testing.T) {
	// Reading a WAL database's main file can hand back a token that the WAL has
	// already replaced, so it is refused rather than served stale.
	_, err := Open(fixture("walmode.sqlite3"))
	if !errors.Is(err, ErrWALPresent) {
		t.Fatalf("error = %v, want ErrWALPresent", err)
	}
	if !strings.Contains(err.Error(), "WAL") {
		t.Errorf("error = %q, should explain the WAL problem", err)
	}
}

func TestOpenRefusesWALSidecar(t *testing.T) {
	// A journal_mode=DELETE database with a stray -wal beside it is still suspect.
	dir := t.TempDir()
	main := filepath.Join(dir, "db.sqlite3")
	data, err := os.ReadFile(fixture("kirocli.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main+"-wal", []byte("wal contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = Open(main)
	if !errors.Is(err, ErrWALPresent) {
		t.Fatalf("error = %v, want ErrWALPresent", err)
	}
	if !strings.Contains(err.Error(), "Close kiro-cli") {
		t.Errorf("error = %q, should say what to do about it", err)
	}
}

func TestLookupFindsValues(t *testing.T) {
	db := openFixture(t, "kirocli.sqlite3")

	value, err := db.Lookup("auth_kv", "key", "value", "kirocli:social:token")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !strings.Contains(value.Text(), "cli-access-token") {
		t.Errorf("value = %q", value.Text())
	}

	profile, err := db.Lookup("state", "key", "value", "api.codewhisperer.profile")
	if err != nil {
		t.Fatalf("Lookup state: %v", err)
	}
	if !strings.Contains(profile.Text(), "FROMSTATE") {
		t.Errorf("state value = %q", profile.Text())
	}
}

func TestLookupMissingKey(t *testing.T) {
	db := openFixture(t, "kirocli.sqlite3")
	_, err := db.Lookup("auth_kv", "key", "value", "no-such-key")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestLookupMissingTable(t *testing.T) {
	db := openFixture(t, "kirocli.sqlite3")
	_, err := db.Lookup("no_such_table", "key", "value", "anything")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not in this database") {
		t.Errorf("error = %q", err)
	}
}

func TestLookupMissingColumn(t *testing.T) {
	db := openFixture(t, "kirocli.sqlite3")
	_, err := db.Lookup("auth_kv", "key", "no_such_column", "kirocli:social:token")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "does not have both") {
		t.Errorf("error = %q, should name the missing column", err)
	}
}

func TestLookupOnEmptyDatabase(t *testing.T) {
	db := openFixture(t, "empty.sqlite3")
	if _, err := db.Lookup("auth_kv", "key", "value", "x"); err == nil {
		t.Error("expected an error when the table does not exist")
	}
	tables, err := db.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	if len(tables) != 0 {
		t.Errorf("Tables() = %v, want none", tables)
	}
}

func TestLookupOnTableWithNoRows(t *testing.T) {
	db := openFixture(t, "norows.sqlite3")
	_, err := db.Lookup("auth_kv", "key", "value", "x")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestOverflowPayloads(t *testing.T) {
	db := openFixture(t, "overflow.sqlite3")
	if db.PageSize() != 512 {
		t.Fatalf("PageSize() = %d, want the fixture's 512", db.PageSize())
	}

	cases := []struct {
		key      string
		wantChar byte
		wantSize int
	}{
		{"one-page", 'A', 400},
		{"spills", 'B', 5000},
		{"spills-far", 'C', 200000},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			value, err := db.Lookup("auth_kv", "key", "value", tc.key)
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			text := value.Text()
			if len(text) != tc.wantSize {
				t.Fatalf("length = %d, want %d", len(text), tc.wantSize)
			}
			// Every byte must be intact, which is what proves the chain was
			// reassembled in order with no gaps or duplication.
			for i := 0; i < len(text); i++ {
				if text[i] != tc.wantChar {
					t.Fatalf("byte %d = %q, want %q: the overflow chain is corrupt",
						i, text[i], tc.wantChar)
				}
			}
		})
	}

	small, err := db.Lookup("auth_kv", "key", "value", "small")
	if err != nil {
		t.Fatal(err)
	}
	if small.Text() != "tiny" {
		t.Errorf("small value = %q", small.Text())
	}
}

func TestMultiPageTableWithInteriorNodes(t *testing.T) {
	db := openFixture(t, "manyrows.sqlite3")

	// Spot-check rows spread across the tree, including both ends.
	for _, i := range []int{0, 1, 500, 999, 1000, 1500, 1999} {
		key := fmt.Sprintf("key-%05d", i)
		value, err := db.Lookup("auth_kv", "key", "value", key)
		if err != nil {
			t.Fatalf("Lookup(%s): %v", key, err)
		}
		want := fmt.Sprintf("value-%05d-", i) + strings.Repeat("z", 80)
		if value.Text() != want {
			t.Errorf("%s = %q, want %q", key, value.Text(), want)
		}
	}

	// A full scan must see every row exactly once.
	schema, err := db.findTable("auth_kv")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	rowids := map[int64]int{}
	err = db.scanTable(schema.rootPage, func(rowid int64, values []Value) (bool, error) {
		seen[values[0].Text()]++
		rowids[rowid]++
		return true, nil
	})
	if err != nil {
		t.Fatalf("scanTable: %v", err)
	}
	if len(seen) != 2001 {
		t.Errorf("scan saw %d distinct keys, want 2001", len(seen))
	}
	for key, count := range seen {
		if count != 1 {
			t.Errorf("key %q was visited %d times", key, count)
		}
	}
	if len(rowids) != 2001 {
		t.Errorf("scan saw %d distinct rowids, want 2001", len(rowids))
	}
}

func TestScanStopsEarly(t *testing.T) {
	db := openFixture(t, "manyrows.sqlite3")
	schema, err := db.findTable("auth_kv")
	if err != nil {
		t.Fatal(err)
	}

	visits := 0
	err = db.scanTable(schema.rootPage, func(int64, []Value) (bool, error) {
		visits++
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if visits != 1 {
		t.Errorf("visited %d rows, want the scan to stop after the first", visits)
	}
}

func TestScanPropagatesVisitorErrors(t *testing.T) {
	db := openFixture(t, "kirocli.sqlite3")
	schema, err := db.findTable("auth_kv")
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("visitor gave up")
	err = db.scanTable(schema.rootPage, func(int64, []Value) (bool, error) {
		return true, want
	})
	if !errors.Is(err, want) {
		t.Errorf("error = %v, want the visitor's error", err)
	}
}

func TestLargerPageSize(t *testing.T) {
	db := openFixture(t, "page16k.sqlite3")
	if db.PageSize() != 16384 {
		t.Errorf("PageSize() = %d, want 16384", db.PageSize())
	}
	value, err := db.Lookup("auth_kv", "key", "value", "huge")
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Text()) != 40000 {
		t.Errorf("length = %d, want 40000", len(value.Text()))
	}
}

func TestEveryValueType(t *testing.T) {
	db := openFixture(t, "types.sqlite3")

	cases := []struct {
		key      string
		wantType ValueType
		check    func(t *testing.T, v Value)
	}{
		{"null", ValueNull, func(t *testing.T, v Value) {
			if !v.IsNull() {
				t.Error("want NULL")
			}
			if v.Text() != "" {
				t.Errorf("Text() = %q, want empty for NULL", v.Text())
			}
		}},
		{"int-zero", ValueInt, func(t *testing.T, v Value) { wantInt(t, v, 0) }},
		{"int-one", ValueInt, func(t *testing.T, v Value) { wantInt(t, v, 1) }},
		{"int-small", ValueInt, func(t *testing.T, v Value) { wantInt(t, v, 42) }},
		{"int-negative", ValueInt, func(t *testing.T, v Value) { wantInt(t, v, -42) }},
		{"int-16", ValueInt, func(t *testing.T, v Value) { wantInt(t, v, 30000) }},
		{"int-24", ValueInt, func(t *testing.T, v Value) { wantInt(t, v, 8000000) }},
		{"int-32", ValueInt, func(t *testing.T, v Value) { wantInt(t, v, 2000000000) }},
		{"int-48", ValueInt, func(t *testing.T, v Value) { wantInt(t, v, 100000000000) }},
		{"int-64", ValueInt, func(t *testing.T, v Value) { wantInt(t, v, 9000000000000000000) }},
		{"int-neg-64", ValueInt, func(t *testing.T, v Value) { wantInt(t, v, -9000000000000000000) }},
		{"float", ValueFloat, func(t *testing.T, v Value) {
			if v.Float != 3.5 {
				t.Errorf("Float = %v, want 3.5", v.Float)
			}
		}},
		{"float-negative", ValueFloat, func(t *testing.T, v Value) {
			if v.Float != -0.125 {
				t.Errorf("Float = %v, want -0.125", v.Float)
			}
		}},
		{"text", ValueText, func(t *testing.T, v Value) {
			if v.Text() != "hello world" {
				t.Errorf("Text() = %q", v.Text())
			}
		}},
		{"text-unicode", ValueText, func(t *testing.T, v Value) {
			if v.Text() != "üñîçød\u00e9 你好" {
				t.Errorf("Text() = %q, multi-byte text did not round-trip", v.Text())
			}
		}},
		{"text-empty", ValueText, func(t *testing.T, v Value) {
			if v.Text() != "" {
				t.Errorf("Text() = %q, want empty", v.Text())
			}
		}},
		{"blob", ValueBlob, func(t *testing.T, v Value) {
			want := []byte{0, 1, 2, 255, 254}
			if !reflect.DeepEqual(v.Bytes, want) {
				t.Errorf("Bytes = %v, want %v", v.Bytes, want)
			}
		}},
		{"blob-empty", ValueBlob, func(t *testing.T, v Value) {
			if len(v.Bytes) != 0 {
				t.Errorf("Bytes = %v, want empty", v.Bytes)
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			value, err := db.Lookup("vals", "key", "value", tc.key)
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if value.Type != tc.wantType {
				t.Errorf("Type = %v, want %v", value.Type, tc.wantType)
			}
			tc.check(t, value)
		})
	}
}

// wantInt asserts an integer value.
func wantInt(t *testing.T, v Value, want int64) {
	t.Helper()
	if v.Int != want {
		t.Errorf("Int = %d, want %d", v.Int, want)
	}
}

func TestColumnOrderIsReadFromTheSchema(t *testing.T) {
	// This fixture puts value first and key third. A reader that assumed the
	// usual order would return the wrong column.
	db := openFixture(t, "column-order.sqlite3")
	value, err := db.Lookup("auth_kv", "key", "value", "kirocli:social:token")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !strings.Contains(value.Text(), "reordered-access") {
		t.Errorf("value = %q, want the value column regardless of its position", value.Text())
	}
}

func TestReadVarint(t *testing.T) {
	cases := []struct {
		name     string
		bytes    []byte
		want     int64
		wantSize int
	}{
		{"zero", []byte{0x00}, 0, 1},
		{"one", []byte{0x01}, 1, 1},
		{"max single byte", []byte{0x7f}, 127, 1},
		{"two bytes", []byte{0x81, 0x00}, 128, 2},
		{"two bytes max", []byte{0xff, 0x7f}, 16383, 2},
		{"three bytes", []byte{0x81, 0x80, 0x00}, 16384, 3},
		{"nine bytes uses all eight bits of the last",
			[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, -1, 9},
		{"trailing bytes are ignored", []byte{0x01, 0xff, 0xff}, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, size, err := readVarint(tc.bytes)
			if err != nil {
				t.Fatalf("readVarint: %v", err)
			}
			if got != tc.want {
				t.Errorf("value = %d, want %d", got, tc.want)
			}
			if size != tc.wantSize {
				t.Errorf("size = %d, want %d", size, tc.wantSize)
			}
		})
	}
}

func TestReadVarintTruncated(t *testing.T) {
	cases := [][]byte{
		{},
		{0x81},
		{0xff, 0xff},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, // eight continuation bytes, no ninth
	}
	for i, b := range cases {
		if _, _, err := readVarint(b); err == nil {
			t.Errorf("case %d: readVarint(%v) should fail on a truncated buffer", i, b)
		}
	}
}

func TestDecodeRecordRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"empty", []byte{}},
		{"header longer than the payload", []byte{0x7f, 0x01}},
		{"header shorter than its own varint", []byte{0x00}},
		{"column runs past the record", []byte{0x02, 0x09 + 0x40}}, // a long text with no body
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeRecord(tc.payload); err == nil {
				t.Errorf("decodeRecord(%v) should fail", tc.payload)
			}
		})
	}
}

func TestDecodeColumnRejectsReservedSerialTypes(t *testing.T) {
	for _, serial := range []int64{10, 11} {
		if _, _, err := decodeColumn(serial, make([]byte, 16)); err == nil {
			t.Errorf("serial type %d is reserved and must be rejected", serial)
		}
	}
}

func TestParseColumnNames(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{
			name: "simple",
			sql:  "CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value BLOB)",
			want: []string{"key", "value"},
		},
		{
			name: "table constraint is skipped",
			sql:  `CREATE TABLE t (a TEXT, b BLOB, PRIMARY KEY (a))`,
			want: []string{"a", "b"},
		},
		{
			name: "named constraint is skipped",
			sql:  `CREATE TABLE t (a TEXT, CONSTRAINT pk PRIMARY KEY (a))`,
			want: []string{"a"},
		},
		{
			name: "foreign key is skipped",
			sql:  `CREATE TABLE t (a TEXT, b INT, FOREIGN KEY (b) REFERENCES o(id))`,
			want: []string{"a", "b"},
		},
		{
			name: "unique and check are skipped",
			sql:  `CREATE TABLE t (a TEXT, UNIQUE (a), CHECK (length(a) > 0))`,
			want: []string{"a"},
		},
		{
			name: "quoted identifiers",
			sql:  `CREATE TABLE t ("key" TEXT, [value] BLOB, ` + "`third`" + ` INT)`,
			want: []string{"key", "value", "third"},
		},
		{
			name: "nested parentheses in a default",
			sql:  `CREATE TABLE t (a TEXT DEFAULT (datetime('now', 'utc')), b INT)`,
			want: []string{"a", "b"},
		},
		{
			name: "types with a precision",
			sql:  `CREATE TABLE t (a VARCHAR(255), b DECIMAL(10, 2))`,
			want: []string{"a", "b"},
		},
		{
			name: "newlines and extra spacing",
			sql:  "CREATE TABLE t (\n  a  TEXT,\n\n  b   BLOB\n)",
			want: []string{"a", "b"},
		},
		{
			name: "no parentheses at all",
			sql:  "CREATE TABLE t",
			want: nil,
		},
		{
			name: "unbalanced parentheses",
			sql:  "CREATE TABLE t (a TEXT",
			want: nil,
		},
		{
			name: "empty statement",
			sql:  "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseColumnNames(tc.sql)
			if len(got) != len(tc.want) {
				t.Fatalf("columns = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("column %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestIndexOfColumnIsCaseInsensitive(t *testing.T) {
	columns := []string{"Key", "VALUE"}
	if got := indexOfColumn(columns, "key"); got != 0 {
		t.Errorf("indexOfColumn(key) = %d, want 0", got)
	}
	if got := indexOfColumn(columns, "value"); got != 1 {
		t.Errorf("indexOfColumn(value) = %d, want 1", got)
	}
	if got := indexOfColumn(columns, "absent"); got != -1 {
		t.Errorf("indexOfColumn(absent) = %d, want -1", got)
	}
}

func TestValueText(t *testing.T) {
	cases := []struct {
		value Value
		want  string
	}{
		{Value{Type: ValueText, Bytes: []byte("abc")}, "abc"},
		{Value{Type: ValueBlob, Bytes: []byte("raw")}, "raw"},
		{Value{Type: ValueInt, Int: -7}, "-7"},
		{Value{Type: ValueFloat, Float: 1.5}, "1.5"},
		{Value{Type: ValueNull}, ""},
	}
	for _, tc := range cases {
		if got := tc.value.Text(); got != tc.want {
			t.Errorf("Text() for %v = %q, want %q", tc.value.Type, got, tc.want)
		}
	}
}

func TestPageBoundsAreChecked(t *testing.T) {
	db := openFixture(t, "kirocli.sqlite3")
	if _, err := db.page(0); err == nil {
		t.Error("page 0 does not exist and must be rejected")
	}
	if _, err := db.page(-1); err == nil {
		t.Error("a negative page number must be rejected")
	}
	if _, err := db.page(1 << 20); err == nil {
		t.Error("a page past the end of the file must be rejected")
	}
	if _, err := db.page(1); err != nil {
		t.Errorf("page 1 should exist: %v", err)
	}
}

func TestIndexPageIsRejected(t *testing.T) {
	// auth_kv has a TEXT PRIMARY KEY, so SQLite creates an index B-tree for it.
	// Handing that root to the table scanner must fail loudly rather than produce
	// nonsense rows.
	db := openFixture(t, "kirocli.sqlite3")

	var indexRoot int
	err := db.scanTable(1, func(_ int64, values []Value) (bool, error) {
		if len(values) > schemaColRootPage && values[schemaColType].Text() == "index" {
			if values[schemaColRootPage].Type == ValueInt {
				indexRoot = int(values[schemaColRootPage].Int)
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if indexRoot == 0 {
		t.Skip("this fixture has no index B-tree")
	}

	err = db.scanTable(indexRoot, func(int64, []Value) (bool, error) { return true, nil })
	if err == nil {
		t.Error("scanning an index root as a table must fail")
	} else if !strings.Contains(err.Error(), "index page") {
		t.Errorf("error = %q, want it to name the problem", err)
	}
}

func TestNewDBRejectsUnknownTextEncoding(t *testing.T) {
	data, err := os.ReadFile(fixture("kirocli.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	corrupt := make([]byte, len(data))
	copy(corrupt, data)
	// Text encoding 99 is not one of the three SQLite defines.
	corrupt[offTextEncoding] = 0
	corrupt[offTextEncoding+1] = 0
	corrupt[offTextEncoding+2] = 0
	corrupt[offTextEncoding+3] = 99

	if _, err := newDB(corrupt); err == nil {
		t.Error("an unknown text encoding must be rejected")
	}
}

func TestNewDBRejectsExcessiveReservedSpace(t *testing.T) {
	// A 255-byte reserve is legal on a large page. It only becomes invalid when it
	// leaves less than the 480 usable bytes the format requires, so this uses the
	// smallest legal page size.
	header := make([]byte, headerSize)
	copy(header, headerMagic)
	header[offPageSize] = 0x02 // 512
	header[offPageSize+1] = 0x00
	header[offReservedSpace] = 255 // 512 - 255 = 257 usable, below the minimum

	if _, err := newDB(header); err == nil {
		t.Error("a reserve that leaves too little usable space must be rejected")
	} else if !strings.Contains(err.Error(), "usable page size") {
		t.Errorf("error = %q, want it to name the usable size", err)
	}
}

func TestNewDBAcceptsALegalReserve(t *testing.T) {
	data, err := os.ReadFile(fixture("kirocli.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	adjusted := make([]byte, len(data))
	copy(adjusted, data)
	adjusted[offReservedSpace] = 32 // legal on a 4096-byte page

	db, err := newDB(adjusted)
	if err != nil {
		t.Fatalf("a legal reserve should be accepted: %v", err)
	}
	if db.usableSize != 4096-32 {
		t.Errorf("usableSize = %d, want the page size minus the reserve", db.usableSize)
	}
}

func TestLocalPayloadSizeStaysWithinBounds(t *testing.T) {
	// localPayloadSize is only called for payloads that exceed the local maximum,
	// which is the precondition the caller enforces.
	for _, pageSize := range []int{512, 1024, 4096, 16384, 65536} {
		db := &DB{pageSize: pageSize, usableSize: pageSize}
		maxLocal := pageSize - 35
		minLocal := ((pageSize - 12) * 32 / 255) - 23

		for _, payload := range []int{maxLocal + 1, maxLocal + 100, maxLocal * 2, maxLocal * 37, 1 << 22} {
			if payload <= maxLocal {
				continue
			}
			got := db.localPayloadSize(payload)
			if got < minLocal || got > maxLocal {
				t.Errorf("page %d payload %d: local size %d is outside [%d, %d]",
					pageSize, payload, got, minLocal, maxLocal)
			}
			if got > payload {
				t.Errorf("page %d payload %d: local size %d exceeds the payload",
					pageSize, payload, got)
			}
		}
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
