// Package store contains a minimal, read-only SQLite reader, used to read
// credentials out of a kiro-cli database without taking on a dependency.
//
// It implements only what that job needs: the file header, table B-tree pages,
// record decoding and overflow chains. There is no query planner, no index
// support and no write path. Nothing here ever modifies the file.
package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
)

// headerMagic is the 16-byte signature every SQLite database starts with.
var headerMagic = []byte("SQLite format 3\x00")

// Layout constants from the SQLite file format.
const (
	headerSize = 100

	// Offsets within the 100-byte header.
	offPageSize      = 16
	offWriteVersion  = 18
	offReadVersion   = 19
	offReservedSpace = 20
	offTextEncoding  = 56

	// Page type markers, the first byte of a page's B-tree header.
	pageIndexInterior = 0x02
	pageTableInterior = 0x05
	pageIndexLeaf     = 0x0a
	pageTableLeaf     = 0x0d

	// walFormatVersion appears in the header when the database is in WAL mode.
	walFormatVersion = 2

	// maxPageSize is SQLite's largest legal page size.
	maxPageSize = 65536
	// minPageSize is SQLite's smallest legal page size.
	minPageSize = 512

	// maxOverflowPages bounds an overflow chain, so a corrupt file cannot make
	// the reader loop forever.
	maxOverflowPages = 100000
	// maxTreeDepth bounds B-tree descent for the same reason.
	maxTreeDepth = 64
)

// ErrWALPresent reports that the database has an active write-ahead log.
//
// The reader deliberately refuses these: the committed pages in the main file
// can be older than the contents of the WAL, so reading it would hand back a
// stale token that has already been replaced.
var ErrWALPresent = errors.New("the database is in WAL mode, so the main file may be stale")

// ErrNotFound reports that a key is absent.
var ErrNotFound = errors.New("key not found")

// DB is an open, read-only SQLite database held in memory.
//
// kiro-cli databases are small, so the whole file is read at once. That avoids
// any chance of observing a half-written page mid-read.
type DB struct {
	data       []byte
	pageSize   int
	usableSize int
	pageCount  int
}

// Open reads and validates a SQLite database file.
func Open(path string) (*DB, error) {
	// Refuse before reading anything if a sidecar WAL exists.
	if _, err := os.Stat(path + "-wal"); err == nil {
		return nil, fmt.Errorf("%w: %s-wal exists. Close kiro-cli so it checkpoints, then try again", ErrWALPresent, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	return newDB(data)
}

// newDB validates a database image and prepares it for reading.
func newDB(data []byte) (*DB, error) {
	if len(data) < headerSize {
		return nil, fmt.Errorf("file is %d bytes, too short to be a SQLite database", len(data))
	}
	if !hasPrefix(data, headerMagic) {
		return nil, errors.New("file does not start with the SQLite signature, so it is not a SQLite database")
	}

	// A stored page size of 1 means 65536, which does not fit in the u16 field.
	raw := binary.BigEndian.Uint16(data[offPageSize:])
	pageSize := int(raw)
	if raw == 1 {
		pageSize = maxPageSize
	}
	if pageSize < minPageSize || pageSize > maxPageSize || pageSize&(pageSize-1) != 0 {
		return nil, fmt.Errorf("page size %d is not a power of two between %d and %d", pageSize, minPageSize, maxPageSize)
	}

	if data[offWriteVersion] == walFormatVersion || data[offReadVersion] == walFormatVersion {
		return nil, fmt.Errorf("%w: the file header declares WAL mode", ErrWALPresent)
	}

	reserved := int(data[offReservedSpace])
	usable := pageSize - reserved
	if usable < 480 {
		return nil, fmt.Errorf("usable page size %d is too small to be valid", usable)
	}

	if encoding := binary.BigEndian.Uint32(data[offTextEncoding:]); encoding > 3 {
		return nil, fmt.Errorf("text encoding %d is not one of the three SQLite defines", encoding)
	}

	return &DB{
		data:       data,
		pageSize:   pageSize,
		usableSize: usable,
		pageCount:  len(data) / pageSize,
	}, nil
}

// hasPrefix reports whether b starts with prefix.
func hasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := range prefix {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}

// PageSize returns the database page size.
func (db *DB) PageSize() int { return db.pageSize }

// page returns page n, which is numbered from 1.
func (db *DB) page(n int) ([]byte, error) {
	if n < 1 {
		return nil, fmt.Errorf("page number %d is invalid", n)
	}
	start := (n - 1) * db.pageSize
	end := start + db.pageSize
	if end > len(db.data) {
		return nil, fmt.Errorf("page %d runs past the end of the file", n)
	}
	return db.data[start:end], nil
}

// ValueType is the storage class of a decoded column.
type ValueType int

// Storage classes.
const (
	ValueNull ValueType = iota
	ValueInt
	ValueFloat
	ValueText
	ValueBlob
)

// Value is one decoded column.
type Value struct {
	Type  ValueType
	Int   int64
	Float float64
	Bytes []byte
}

// Text returns the value as a string. A blob is interpreted as UTF-8, which is
// how kiro-cli stores its JSON payloads.
func (v Value) Text() string {
	switch v.Type {
	case ValueText, ValueBlob:
		return string(v.Bytes)
	case ValueInt:
		return fmt.Sprint(v.Int)
	case ValueFloat:
		return fmt.Sprint(v.Float)
	default:
		return ""
	}
}

// IsNull reports whether the column holds SQL NULL.
func (v Value) IsNull() bool { return v.Type == ValueNull }

// readVarint decodes a SQLite variable-length integer.
//
// Bytes contribute seven bits each, most significant first, until a byte with
// the high bit clear. A ninth byte contributes all eight of its bits.
func readVarint(b []byte) (value int64, size int, err error) {
	var result uint64
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return 0, 0, errors.New("varint runs past the end of the buffer")
		}
		result = result<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return int64(result), i + 1, nil
		}
	}
	if len(b) < 9 {
		return 0, 0, errors.New("varint runs past the end of the buffer")
	}
	result = result<<8 | uint64(b[8])
	return int64(result), 9, nil
}

// decodeRecord decodes a record payload into its columns.
func decodeRecord(payload []byte) ([]Value, error) {
	headerLen, headerLenSize, err := readVarint(payload)
	if err != nil {
		return nil, fmt.Errorf("record header length: %w", err)
	}
	if headerLen < int64(headerLenSize) || headerLen > int64(len(payload)) {
		return nil, fmt.Errorf("record header length %d does not fit in a %d byte payload", headerLen, len(payload))
	}

	// Serial types occupy the header after its own length varint.
	var serialTypes []int64
	pos := headerLenSize
	for pos < int(headerLen) {
		serial, size, err := readVarint(payload[pos:])
		if err != nil {
			return nil, fmt.Errorf("record serial type: %w", err)
		}
		serialTypes = append(serialTypes, serial)
		pos += size
	}

	body := payload[headerLen:]
	values := make([]Value, 0, len(serialTypes))
	offset := 0

	for _, serial := range serialTypes {
		value, size, err := decodeColumn(serial, body[offset:])
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		offset += size
	}
	return values, nil
}

// decodeColumn decodes one column given its serial type.
func decodeColumn(serial int64, body []byte) (Value, int, error) {
	need := func(n int) error {
		if len(body) < n {
			return fmt.Errorf("column of %d bytes runs past the end of the record", n)
		}
		return nil
	}

	switch {
	case serial == 0:
		return Value{Type: ValueNull}, 0, nil

	case serial >= 1 && serial <= 6:
		// Big-endian twos-complement integers of 1, 2, 3, 4, 6 and 8 bytes.
		sizes := map[int64]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8}
		n := sizes[serial]
		if err := need(n); err != nil {
			return Value{}, 0, err
		}
		var raw uint64
		for i := 0; i < n; i++ {
			raw = raw<<8 | uint64(body[i])
		}
		// Sign-extend from the top bit of the stored width.
		shift := uint(64 - 8*n)
		return Value{Type: ValueInt, Int: int64(raw<<shift) >> shift}, n, nil

	case serial == 7:
		if err := need(8); err != nil {
			return Value{}, 0, err
		}
		bits := binary.BigEndian.Uint64(body)
		return Value{Type: ValueFloat, Float: math.Float64frombits(bits)}, 8, nil

	case serial == 8:
		return Value{Type: ValueInt, Int: 0}, 0, nil

	case serial == 9:
		return Value{Type: ValueInt, Int: 1}, 0, nil

	case serial == 10 || serial == 11:
		return Value{}, 0, fmt.Errorf("serial type %d is reserved for internal use", serial)

	case serial >= 12 && serial%2 == 0:
		n := int((serial - 12) / 2)
		if err := need(n); err != nil {
			return Value{}, 0, err
		}
		out := make([]byte, n)
		copy(out, body[:n])
		return Value{Type: ValueBlob, Bytes: out}, n, nil

	default: // serial >= 13, odd
		n := int((serial - 13) / 2)
		if err := need(n); err != nil {
			return Value{}, 0, err
		}
		out := make([]byte, n)
		copy(out, body[:n])
		return Value{Type: ValueText, Bytes: out}, n, nil
	}
}

// rowVisitor is called for each row of a table scan. Returning false stops the
// scan early.
type rowVisitor func(rowid int64, values []Value) (bool, error)

// scanTable walks a table B-tree, visiting every row in key order.
func (db *DB) scanTable(rootPage int, visit rowVisitor) error {
	_, err := db.scanNode(rootPage, visit, 0)
	return err
}

// scanNode walks one B-tree node, recursing into children.
func (db *DB) scanNode(pageNum int, visit rowVisitor, depth int) (bool, error) {
	if depth > maxTreeDepth {
		return false, fmt.Errorf("B-tree is deeper than %d levels, which suggests a cycle", maxTreeDepth)
	}

	page, err := db.page(pageNum)
	if err != nil {
		return false, err
	}

	// Page 1 carries the 100-byte database header before its B-tree header.
	offset := 0
	if pageNum == 1 {
		offset = headerSize
	}
	if offset+12 > len(page) {
		return false, fmt.Errorf("page %d is too small to hold a B-tree header", pageNum)
	}

	pageType := page[offset]
	cellCount := int(binary.BigEndian.Uint16(page[offset+3:]))

	// An interior page's header is 12 bytes, a leaf's is 8.
	headerBytes := 8
	if pageType == pageTableInterior || pageType == pageIndexInterior {
		headerBytes = 12
	}
	pointerArray := offset + headerBytes

	switch pageType {
	case pageTableLeaf:
		for i := 0; i < cellCount; i++ {
			cellPointer := pointerArray + i*2
			if cellPointer+2 > len(page) {
				return false, fmt.Errorf("page %d cell pointer %d runs past the page", pageNum, i)
			}
			cellStart := int(binary.BigEndian.Uint16(page[cellPointer:]))
			if cellStart >= len(page) {
				return false, fmt.Errorf("page %d cell %d starts past the page", pageNum, i)
			}

			rowid, payload, err := db.readTableLeafCell(page[cellStart:])
			if err != nil {
				return false, fmt.Errorf("page %d cell %d: %w", pageNum, i, err)
			}
			values, err := decodeRecord(payload)
			if err != nil {
				return false, fmt.Errorf("page %d cell %d: %w", pageNum, i, err)
			}
			keepGoing, err := visit(rowid, values)
			if err != nil || !keepGoing {
				return false, err
			}
		}
		return true, nil

	case pageTableInterior:
		for i := 0; i < cellCount; i++ {
			cellPointer := pointerArray + i*2
			if cellPointer+2 > len(page) {
				return false, fmt.Errorf("page %d cell pointer %d runs past the page", pageNum, i)
			}
			cellStart := int(binary.BigEndian.Uint16(page[cellPointer:]))
			if cellStart+4 > len(page) {
				return false, fmt.Errorf("page %d cell %d starts past the page", pageNum, i)
			}
			child := int(binary.BigEndian.Uint32(page[cellStart:]))
			keepGoing, err := db.scanNode(child, visit, depth+1)
			if err != nil || !keepGoing {
				return false, err
			}
		}
		// The rightmost child pointer lives in the page header.
		right := int(binary.BigEndian.Uint32(page[offset+8:]))
		if right != 0 {
			return db.scanNode(right, visit, depth+1)
		}
		return true, nil

	case pageIndexLeaf, pageIndexInterior:
		// Every row is reachable through the table B-tree, so index pages are
		// never needed. Reaching one means the caller passed an index root.
		return false, fmt.Errorf("page %d is an index page, not a table page", pageNum)

	default:
		return false, fmt.Errorf("page %d has unknown type %#02x", pageNum, pageType)
	}
}

// readTableLeafCell reads one table leaf cell, following any overflow chain.
func (db *DB) readTableLeafCell(cell []byte) (rowid int64, payload []byte, err error) {
	payloadSize, n1, err := readVarint(cell)
	if err != nil {
		return 0, nil, fmt.Errorf("payload size: %w", err)
	}
	rowid, n2, err := readVarint(cell[n1:])
	if err != nil {
		return 0, nil, fmt.Errorf("rowid: %w", err)
	}
	body := cell[n1+n2:]

	if payloadSize < 0 {
		return 0, nil, fmt.Errorf("negative payload size %d", payloadSize)
	}

	// A payload larger than the local maximum spills onto overflow pages.
	maxLocal := db.usableSize - 35
	if int(payloadSize) <= maxLocal {
		if int(payloadSize) > len(body) {
			return 0, nil, fmt.Errorf("payload of %d bytes does not fit in the cell", payloadSize)
		}
		return rowid, body[:payloadSize], nil
	}

	localSize := db.localPayloadSize(int(payloadSize))
	if localSize+4 > len(body) {
		return 0, nil, fmt.Errorf("local payload of %d bytes plus an overflow pointer does not fit in the cell", localSize)
	}

	out := make([]byte, 0, payloadSize)
	out = append(out, body[:localSize]...)
	nextPage := int(binary.BigEndian.Uint32(body[localSize:]))

	for pages := 0; nextPage != 0 && len(out) < int(payloadSize); pages++ {
		if pages > maxOverflowPages {
			return 0, nil, fmt.Errorf("overflow chain is longer than %d pages, which suggests a cycle", maxOverflowPages)
		}
		page, err := db.page(nextPage)
		if err != nil {
			return 0, nil, fmt.Errorf("overflow page %d: %w", nextPage, err)
		}
		// The first four bytes of an overflow page point at the next one.
		chunk := page[4:db.usableSize]
		remaining := int(payloadSize) - len(out)
		if remaining < len(chunk) {
			chunk = chunk[:remaining]
		}
		out = append(out, chunk...)
		nextPage = int(binary.BigEndian.Uint32(page[0:4]))
	}

	if len(out) != int(payloadSize) {
		return 0, nil, fmt.Errorf("overflow chain yielded %d of %d payload bytes", len(out), payloadSize)
	}
	return rowid, out, nil
}

// localPayloadSize computes how much of an oversized payload stays in the cell.
//
// This is the formula from the SQLite file format: it keeps the local part large
// enough to be useful while guaranteeing at least four cells fit on a page.
func (db *DB) localPayloadSize(payloadSize int) int {
	u := db.usableSize
	maxLocal := u - 35
	minLocal := ((u - 12) * 32 / 255) - 23

	k := minLocal + (payloadSize-minLocal)%(u-4)
	if k <= maxLocal {
		return k
	}
	return minLocal
}

// schemaTable describes one table found in the schema.
type schemaTable struct {
	name     string
	rootPage int
	sql      string
}

// Columns in the sqlite_schema record, which has a fixed layout.
const (
	schemaColType     = 0
	schemaColName     = 1
	schemaColRootPage = 3
	schemaColSQL      = 4
)

// findTable looks a table up in the schema, which is the B-tree rooted at page 1.
func (db *DB) findTable(name string) (*schemaTable, error) {
	var found *schemaTable

	err := db.scanTable(1, func(_ int64, values []Value) (bool, error) {
		if len(values) <= schemaColSQL {
			return true, nil
		}
		if values[schemaColType].Text() != "table" {
			return true, nil
		}
		if !strings.EqualFold(values[schemaColName].Text(), name) {
			return true, nil
		}
		root := values[schemaColRootPage]
		if root.Type != ValueInt || root.Int < 1 {
			return false, fmt.Errorf("table %q has an invalid root page", name)
		}
		found = &schemaTable{
			name:     values[schemaColName].Text(),
			rootPage: int(root.Int),
			sql:      values[schemaColSQL].Text(),
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("table %q is not in this database", name)
	}
	return found, nil
}

// Tables lists the table names in the database, for diagnostics.
func (db *DB) Tables() ([]string, error) {
	var names []string
	err := db.scanTable(1, func(_ int64, values []Value) (bool, error) {
		if len(values) > schemaColName && values[schemaColType].Text() == "table" {
			names = append(names, values[schemaColName].Text())
		}
		return true, nil
	})
	return names, err
}

// Lookup returns the value column of the row whose key column matches key.
//
// It is the whole query surface this reader needs: kiro-cli stores credentials as
// key/value rows in small tables, so a scan is both simpler and fast enough.
// Column positions come from the table's CREATE statement rather than being
// assumed, so a schema change does not silently read the wrong column.
func (db *DB) Lookup(table, keyColumn, valueColumn, key string) (Value, error) {
	schema, err := db.findTable(table)
	if err != nil {
		return Value{}, err
	}

	columns := parseColumnNames(schema.sql)
	keyIndex := indexOfColumn(columns, keyColumn)
	valueIndex := indexOfColumn(columns, valueColumn)
	if keyIndex < 0 || valueIndex < 0 {
		return Value{}, fmt.Errorf("table %q does not have both a %q and a %q column (found %v)",
			table, keyColumn, valueColumn, columns)
	}

	var result Value
	var found bool
	err = db.scanTable(schema.rootPage, func(_ int64, values []Value) (bool, error) {
		if keyIndex >= len(values) || valueIndex >= len(values) {
			return true, nil
		}
		if values[keyIndex].Text() != key {
			return true, nil
		}
		result = values[valueIndex]
		found = true
		return false, nil
	})
	if err != nil {
		return Value{}, err
	}
	if !found {
		return Value{}, fmt.Errorf("%w: %q in table %q", ErrNotFound, key, table)
	}
	return result, nil
}

// tableConstraintKeywords begin a table constraint rather than a column.
var tableConstraintKeywords = map[string]bool{
	"primary": true, "unique": true, "check": true,
	"foreign": true, "constraint": true,
}

// parseColumnNames extracts column names from a CREATE TABLE statement.
//
// This is not a SQL parser. It takes the outermost parenthesised list, splits it
// on commas that are not nested, and reads the first identifier of each part,
// skipping table constraints. That covers every schema kiro-cli uses while
// staying small enough to reason about.
func parseColumnNames(createSQL string) []string {
	open := strings.Index(createSQL, "(")
	if open < 0 {
		return nil
	}

	// Find the matching close paren for the opening one.
	depth := 0
	closeIdx := -1
	for i := open; i < len(createSQL); i++ {
		switch createSQL[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				closeIdx = i
			}
		}
		if closeIdx >= 0 {
			break
		}
	}
	if closeIdx < 0 {
		return nil
	}

	body := createSQL[open+1 : closeIdx]

	// Split on top-level commas.
	var parts []string
	depth = 0
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, body[start:])

	var columns []string
	for _, part := range parts {
		name := firstIdentifier(part)
		if name == "" {
			continue
		}
		if tableConstraintKeywords[strings.ToLower(name)] {
			continue
		}
		columns = append(columns, name)
	}
	return columns
}

// firstIdentifier returns the first identifier in a column definition, stripping
// the quoting styles SQLite accepts.
func firstIdentifier(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// Quoted identifiers run to their closing quote.
	switch s[0] {
	case '"', '`', '[', '\'':
		closer := s[0]
		if closer == '[' {
			closer = ']'
		}
		if end := strings.IndexByte(s[1:], closer); end >= 0 {
			return s[1 : 1+end]
		}
		return ""
	}

	// Otherwise it runs to the first space or punctuation.
	end := len(s)
	for i, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '(' || r == ',' {
			end = i
			break
		}
	}
	return s[:end]
}

// indexOfColumn finds a column by name, case-insensitively.
func indexOfColumn(columns []string, name string) int {
	for i, c := range columns {
		if strings.EqualFold(c, name) {
			return i
		}
	}
	return -1
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
