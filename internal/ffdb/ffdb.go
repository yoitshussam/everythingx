package ffdb

import (
	"database/sql"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AlanKK/everythingx/internal/shared"
	"github.com/mattn/go-sqlite3"
)

// TODO:  check out driver benchmarks for sqlite3 at https://github.com/cvilsmeier/go-sqlite-bench

var records []struct {
	filename string
	path     string
}

var prefixSearchStmt *sql.Stmt
var trigramSearchStmt *sql.Stmt
var insertStmt *sql.Stmt
var deleteStmt *sql.Stmt
var deleteDirStmt *sql.Stmt
var renameDirStmt *sql.Stmt
var fullPathLikeStmt *sql.Stmt

// Creates and opens a new database at the given pathname.
func CreateDB(pathname string) (*sql.DB, error) {
	if shared.FileExists(pathname) {
		return nil, os.ErrExist
	}

	db, err := sql.Open("sqlite3", pathname)
	if err != nil {
		return nil, err
	}

	createTablesAndIndexes(db)
	configureDB(db)
	prepareStatements(db)

	return db, err
}

// Opens an existing database
func OpenDB(pathname string) (*sql.DB, error) {
	// Check if the database file exists
	if !shared.FileExists(pathname) {
		return nil, error(os.ErrNotExist)
	}

	db, err := sql.Open("sqlite3", pathname)
	if err != nil {
		return nil, err
	}

	configureDB(db)
	prepareStatements(db)

	return db, err
}

// Open read-only for the UI.  Can't use PRAGMAS in read-only mode.
func OpenDBReadOnly(pathname string) (*sql.DB, error) {
	// Check if the database file exists
	if !shared.FileExists(pathname) {
		return nil, error(os.ErrNotExist)
	}

	db, err := sql.Open("sqlite3", "file:"+pathname+"?mode=ro")
	if err != nil {
		return nil, err
	}

	prepareStatements(db)

	return db, err
}

// Creates the necessary tables and indexes in the database.
func createTablesAndIndexes(db *sql.DB) {
	_, err := db.Exec("CREATE TABLE IF NOT EXISTS files(filename TEXT NOT NULL, fullpath TEXT NOT NULL UNIQUE, event_id INTEGER, object_type INTEGER)")
	if err != nil {
		log.Fatal(err)
	}

	_, err = db.Exec("CREATE INDEX idx_filename ON files(filename COLLATE BINARY, fullpath, object_type)")
	if err != nil {
		log.Fatal(err)
	}

	_, err = db.Exec("CREATE VIRTUAL TABLE files_fts USING fts5(filename, content='files', content_rowid='rowid', tokenize='trigram')")
	if err != nil {
		log.Fatal(err)
	}

	// Triggers to keep the FTS index in sync with the files table.
	_, err = db.Exec(`CREATE TRIGGER files_ai AFTER INSERT ON files BEGIN
		INSERT INTO files_fts(rowid, filename) VALUES (new.rowid, new.filename);
	END`)
	if err != nil {
		log.Fatal(err)
	}
	_, err = db.Exec(`CREATE TRIGGER files_ad AFTER DELETE ON files BEGIN
		INSERT INTO files_fts(files_fts, rowid, filename) VALUES('delete', old.rowid, old.filename);
	END`)
	if err != nil {
		log.Fatal(err)
	}
	_, err = db.Exec(`CREATE TRIGGER files_au AFTER UPDATE ON files BEGIN
		INSERT INTO files_fts(files_fts, rowid, filename) VALUES('delete', old.rowid, old.filename);
		INSERT INTO files_fts(rowid, filename) VALUES (new.rowid, new.filename);
	END`)
	if err != nil {
		log.Fatal(err)
	}
}

// Configures the database with necessary settings.
func configureDB(db *sql.DB) {

	// For the UI to use case sensitive search.
	// _, err := db.Exec("PRAGMA case_sensitive_like = ON")
	// if err != nil {
	// 	log.Fatal(err)
	// }

	// Enable WAL mode - multiple readers and writer
	_, err := db.Exec("PRAGMA journal_mode=WAL;")
	if err != nil {
		log.Fatal(err)
	}

	// No performane impact for this
	_, err = db.Exec("PRAGMA synchronous=NORMAL;")
	if err != nil {
		log.Fatal(err)
	}
}

// Prepares the SQL statements for later use.
func prepareStatements(db *sql.DB) {
	var err error

	prefixSearchStmt, err = db.Prepare("SELECT fullpath, object_type FROM files WHERE filename LIKE ? COLLATE BINARY ORDER BY filename ASC LIMIT ?")
	if err != nil {
		log.Fatal(err)
	}

	trigramSearchStmt, err = db.Prepare("SELECT f.fullpath, f.object_type FROM files_fts fts JOIN files f ON f.rowid = fts.rowid WHERE files_fts MATCH ? ORDER BY f.filename ASC LIMIT ?")
	if err != nil {
		// FTS table may not exist in older databases opened read-only
		log.Printf("Warning: could not prepare trigram search statement: %v", err)
		trigramSearchStmt = nil
	}

	insertStmt, err = db.Prepare("INSERT INTO files (filename, fullpath, event_id, object_type) VALUES (?, ?, ?, ?)")
	if err != nil {
		log.Fatal(err)
	}

	deleteStmt, err = db.Prepare("DELETE FROM files WHERE fullpath = ?")
	if err != nil {
		log.Fatal(err)
	}

	deleteDirStmt, err = db.Prepare("DELETE FROM files WHERE fullpath = ? OR (fullpath > ? AND fullpath < ?)")
	if err != nil {
		log.Fatal(err)
	}

	renameDirStmt, err = db.Prepare("UPDATE files SET fullpath = ? || SUBSTR(fullpath, LENGTH(?) + 1), filename = CASE WHEN fullpath = ? THEN ? ELSE filename END WHERE fullpath = ? OR (fullpath > ? AND fullpath < ?)")
	if err != nil {
		log.Fatal(err)
	}

	fullPathLikeStmt, err = db.Prepare("SELECT fullpath FROM files where fullpath like ?")
	if err != nil {
		log.Fatal(err)
	}
}

// Performs a substring search for filenames containing the given term(s).
// The query is split on whitespace, and every resulting word must appear in the
// filename as a substring, in any order and with anything between them — the
// same behaviour as Everything on Windows (e.g. "transcript signed" matches a
// file named "signed_transcript.pdf").
//
// For terms >= 3 characters, uses the FTS5 trigram index for fast lookup.
// For shorter terms, uses prefix search with the B-tree index.
func PrefixSearch(query string, limit int) ([]*shared.SearchResult, error) {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return nil, nil
	}

	// A single term keeps the original indexed lookup.
	if len(terms) == 1 {
		return prefixSearchSingle(terms[0], limit)
	}

	return prefixSearchMulti(terms, limit)
}

// prefixSearchSingle performs a search for a single term, keeping the original
// behaviour: trigram substring match for terms >= 3 chars, prefix match otherwise.
func prefixSearchSingle(term string, limit int) ([]*shared.SearchResult, error) {
	var rows *sql.Rows
	var err error

	if len(term) >= 3 && trigramSearchStmt != nil {
		// Wrap in double quotes so FTS5 treats the term as a literal phrase,
		// preventing characters like "." from causing syntax errors.
		quoted := `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
		rows, err = trigramSearchStmt.Query(quoted, limit)
	} else {
		rows, err = prefixSearchStmt.Query(term+"%", limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*shared.SearchResult
	for rows.Next() {
		result := &shared.SearchResult{}
		if err := rows.Scan(&result.Fullpath, &result.ObjectType); err != nil {
			return nil, err
		}
		results = append(results, result)
	}

	return results, nil
}

// prefixSearchMulti searches for files whose names contain every one of the
// given terms, in any order. Long terms are combined into a single FTS5 AND
// query so each must appear as a substring; short terms (which the trigram
// tokenizer cannot index) are applied as a case-insensitive substring filter.
func prefixSearchMulti(terms []string, limit int) ([]*shared.SearchResult, error) {
	var longTerms, shortTerms []string
	for _, t := range terms {
		if len(t) >= 3 {
			longTerms = append(longTerms, t)
		} else {
			shortTerms = append(shortTerms, t)
		}
	}

	var results []*shared.SearchResult
	var err error

	if len(longTerms) > 0 && trigramSearchStmt != nil {
		// Join the long terms into a single AND query so every one of them
		// must appear as a substring anywhere in the filename.
		quoted := make([]string, len(longTerms))
		for i, t := range longTerms {
			quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		}
		results, err = queryTrigram(strings.Join(quoted, " AND "))
		if err != nil {
			return nil, err
		}
	} else {
		// No long terms (or no trigram index available): fall back to a prefix
		// match on the first term, matching the single-term fallback behaviour.
		results, err = queryPrefix(terms[0] + "%")
		if err != nil {
			return nil, err
		}
		shortTerms = terms[1:]
	}

	if len(shortTerms) > 0 {
		results = filterBySubstrings(results, shortTerms)
	}

	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// queryTrigram runs a trigram FTS query and returns all matching results.
func queryTrigram(ftsQuery string) ([]*shared.SearchResult, error) {
	rows, err := trigramSearchStmt.Query(ftsQuery, math.MaxInt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSearchResults(rows)
}

// queryPrefix runs a prefix (LIKE) query and returns all matching results.
func queryPrefix(prefix string) ([]*shared.SearchResult, error) {
	rows, err := prefixSearchStmt.Query(prefix, math.MaxInt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSearchResults(rows)
}

// scanSearchResults reads rows into a slice of search results.
func scanSearchResults(rows *sql.Rows) ([]*shared.SearchResult, error) {
	var results []*shared.SearchResult
	for rows.Next() {
		result := &shared.SearchResult{}
		if err := rows.Scan(&result.Fullpath, &result.ObjectType); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

// filterBySubstrings keeps only results whose filename contains every term,
// matching case-insensitively. Used for short terms that the trigram index
// cannot represent.
func filterBySubstrings(results []*shared.SearchResult, terms []string) []*shared.SearchResult {
	out := results[:0]
	for _, r := range results {
		name := strings.ToLower(filepath.Base(r.Fullpath))
		match := true
		for _, term := range terms {
			if !strings.Contains(name, strings.ToLower(term)) {
				match = false
				break
			}
		}
		if match {
			out = append(out, r)
		}
	}
	return out
}

// Returns the count of records in the files table.
func RecordCount(db *sql.DB) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM files").Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// Deletes a record from the database with the given fullpath.
func DeleteRecord(db *sql.DB, fullpath string) error {
	_, err := db.Exec("DELETE FROM files WHERE fullpath = ?", fullpath)
	return err
}

// Inserts a new record into the database.
func InsertRecord(db *sql.DB, filename string, path string, eventID uint64, objectType shared.ObjectType) error {
	_, err := db.Exec("INSERT OR IGNORE INTO files (filename, fullpath, event_id, object_type) VALUES (?, ?, ?, ?)", filename, path, eventID, objectType)
	return err
}

func FullPathLikeQuery(path string) (*[]string, error) {
	var results []string

	rows, err := fullPathLikeStmt.Query(path + "%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var result string
		err = rows.Scan(&result)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}

	return &results, nil
}

// FullPathLikeQueryEach streams matching paths one at a time via the callback,
// avoiding materializing all results into a slice.
func FullPathLikeQueryEach(path string, fn func(string)) error {
	rows, err := fullPathLikeStmt.Query(path + "%")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		fn(result)
	}
	return rows.Err()
}

// Collects records and commits them to the database when enough records are collected.
func BulkInsertRecords(db *sql.DB, filename string, path string) error {

	// Collect records here
	records = append(records, struct {
		filename string
		path     string
	}{filename, path})

	// Commit records when we have enough
	if len(records) >= 1000 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}

		for _, record := range records {
			_, err = insertStmt.Exec(record.filename, record.path, nil, 0)
			if err != nil {
				return err
			}
		}

		err = tx.Commit()
		if err != nil {
			return err
		}

		records = records[:0] // Clear the slice
	}

	return nil
}

// Commits any collected records to the database.
func CommitRecords(db *sql.DB) error {

	if len(records) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare("INSERT INTO files (filename, fullpath) VALUES (?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, record := range records {
		_, err = stmt.Exec(record.filename, record.path)
		if err != nil {
			return err
		}
	}

	err = tx.Commit()
	if err != nil {
		return err
	}

	records = records[:0] // Clear the slice
	return nil
}

// Stores a batch of events in the database.
func BulkStoreEvents(db *sql.DB, eventRecordQueue *[]shared.EventRecord) error {
	if len(*eventRecordQueue) == 0 {
		log.Println("No events to store")
		return nil
	}
	bulkTime := time.Now()

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	var num_committed, num_missing, num_duplicate, num_deleted, num_renamed int

	// Pre-process: detect directory rename pairs.
	// FSEvents/fanotify fire two events for a rename: one for the old path
	// (no longer exists) and one for the new path (now exists).
	handled := make(map[int]bool)
	for i, e := range *eventRecordQueue {
		if !e.IsRename || e.ObjectType != shared.ItemIsDir || e.FoundOnScan || shared.FileExists(e.Path) {
			continue
		}
		// Old path of a dir rename. Find the new-path partner.
		for j := i + 1; j < len(*eventRecordQueue); j++ {
			ej := (*eventRecordQueue)[j]
			if ej.IsRename && ej.ObjectType == shared.ItemIsDir && !handled[j] && shared.FileExists(ej.Path) {
				_, err = renameDirStmt.Exec(ej.Path, e.Path, e.Path, ej.Filename, e.Path, e.Path+"/", e.Path+"0")
				if err != nil {
					return err
				}
				handled[i] = true
				handled[j] = true
				num_renamed++
				break
			}
		}
	}

	for i, e := range *eventRecordQueue {
		if handled[i] {
			continue
		}
		if e.FoundOnScan || shared.FileExists(e.Path) {
			_, err = insertStmt.Exec(e.Filename, e.Path, e.EventID, e.ObjectType)
			if err != nil {
				if isDuplicate(err) {
					num_duplicate++
				} else {
					return err
				}
			}
			num_committed++
		} else {
			if e.ObjectType == shared.ItemIsDir {
				if e.IsRename {
					// Unpaired rename — just remove the dir entry, don't cascade.
					_, err = deleteStmt.Exec(e.Path)
				} else {
					_, err = deleteDirStmt.Exec(e.Path, e.Path+"/", e.Path+"0")
				}
			} else {
				_, err = deleteStmt.Exec(e.Path)
			}
			if err != nil {
				return err
			}
			num_deleted++
		}
	}

	commitTime := time.Now()

	err = tx.Commit()
	if err != nil {
		return err
	}

	commitTimeEnd := time.Since(commitTime).Round(time.Microsecond).String()
	bulkTimeEnd := time.Since(bulkTime).Round(time.Microsecond).String()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	log.Printf(
		"Events: %-6d Del: %-6d New: %-6d Ren: %-6d Missing: %-6d Dups: %-6d Time fn/commit: %-9s / %-8s Queue: %p Capacity: %-7d Mem: %.1f heap %.1f MB",
		len(*eventRecordQueue),
		num_deleted,
		num_committed-num_duplicate,
		num_renamed,
		num_missing,
		num_duplicate,
		bulkTimeEnd,
		commitTimeEnd,
		eventRecordQueue,
		cap(*eventRecordQueue),
		float64(shared.BToMb(m.Sys)),
		float64(shared.BToMb(m.Alloc)),
	)

	return nil
}

// Checks if the given error is a SQLite constraint violation error.
func isDuplicate(err error) bool {
	if sqliteErr, ok := err.(sqlite3.Error); ok && sqliteErr.Code == sqlite3.ErrConstraint {
		return true
	}
	return false
}
