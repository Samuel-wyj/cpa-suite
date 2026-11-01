package suite

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ImportResult records a one-time copy of the cpa-key-billing database.
type ImportResult struct {
	Source          string           `json:"source"`
	Target          string           `json:"target"`
	SourceSHA256    string           `json:"source_sha256"`
	SourceUnchanged bool             `json:"source_unchanged"`
	SchemaVersion   int              `json:"schema_version"`
	Rows            map[string]int64 `json:"rows"`
	ImportedAt      time.Time        `json:"imported_at"`
}

// importedTables are counted before and after the copy as a sanity check.
var importedTables = []string{"api_keys", "plans", "routes", "prices", "config_credentials", "request_events", "request_errors"}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readOnlyDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: path, OmitHost: true}).String() + "?mode=ro&_busy_timeout=5000"
}

func countRows(db *sql.DB) (map[string]int64, error) {
	rows := map[string]int64{}
	for _, table := range importedTables {
		var n int64
		if err := db.QueryRow(`SELECT count(*) FROM "` + table + `"`).Scan(&n); err != nil {
			return nil, fmt.Errorf("Count %s: %w", table, err)
		}
		rows[table] = n
	}
	return rows, nil
}

// ImportDatabase copies source to target without writing to source.
//
// The copy is made with VACUUM INTO from a read-only connection, which yields
// a consistent, self-contained file (including PRAGMA user_version) and folds
// any committed WAL content in. maxSchema is the newest upstream schema this
// build understands; newer sources are refused so a downgrade never happens.
// target must not exist. On any error the partial target is removed.
func ImportDatabase(source, target string, maxSchema int, now time.Time) (ImportResult, error) {
	result := ImportResult{Source: source, Target: target}
	absSource, err := filepath.Abs(source)
	if err != nil {
		return result, err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return result, err
	}
	if absSource == absTarget {
		return result, fmt.Errorf("import_from and state_file must be different files")
	}
	info, err := os.Stat(absSource)
	if err != nil {
		return result, fmt.Errorf("Import source %s: %w", source, err)
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("Import source %s is not a regular file", source)
	}
	if _, err := os.Stat(absTarget); err == nil {
		return result, fmt.Errorf("Import target %s already exists", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	before, err := fileSHA256(absSource)
	if err != nil {
		return result, fmt.Errorf("Hash import source: %w", err)
	}
	result.SourceSHA256 = before

	src, err := sql.Open("sqlite3", readOnlyDSN(absSource))
	if err != nil {
		return result, err
	}
	src.SetMaxOpenConns(1)
	defer src.Close()
	if err := src.QueryRow("PRAGMA user_version").Scan(&result.SchemaVersion); err != nil {
		return result, fmt.Errorf("Read import source (stop CPA so the file is not in use, then retry): %w", err)
	}
	if result.SchemaVersion == 0 {
		return result, fmt.Errorf("Import source %s is not a cpa-key-billing database (user_version 0)", source)
	}
	if result.SchemaVersion > maxSchema {
		return result, fmt.Errorf("Import source schema v%d is newer than this build supports (v%d); upgrade CPA Suite first", result.SchemaVersion, maxSchema)
	}
	var integrity string
	if err := src.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return result, fmt.Errorf("Import source failed integrity check: %v %s", err, integrity)
	}
	sourceRows, err := countRows(src)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(filepath.Dir(absTarget), 0o755); err != nil {
		return result, err
	}
	tmp := absTarget + ".importing"
	_ = os.Remove(tmp)
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := src.Exec("VACUUM INTO ?", tmp); err != nil {
		cleanup()
		return result, fmt.Errorf("Copy import source: %w", err)
	}
	if err := src.Close(); err != nil {
		cleanup()
		return result, err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		cleanup()
		return result, err
	}

	dst, err := sql.Open("sqlite3", readOnlyDSN(tmp))
	if err != nil {
		cleanup()
		return result, err
	}
	var copiedVersion int
	errVerify := dst.QueryRow("PRAGMA user_version").Scan(&copiedVersion)
	var copiedRows map[string]int64
	if errVerify == nil {
		copiedRows, errVerify = countRows(dst)
	}
	_ = dst.Close()
	if errVerify != nil {
		cleanup()
		return result, fmt.Errorf("Verify imported copy: %w", errVerify)
	}
	if copiedVersion != result.SchemaVersion {
		cleanup()
		return result, fmt.Errorf("Imported copy has schema v%d, source v%d", copiedVersion, result.SchemaVersion)
	}
	for table, n := range sourceRows {
		if copiedRows[table] != n {
			cleanup()
			return result, fmt.Errorf("Imported copy row count mismatch in %s: %d != %d", table, copiedRows[table], n)
		}
	}
	after, err := fileSHA256(absSource)
	if err != nil {
		cleanup()
		return result, err
	}
	result.SourceUnchanged = after == before
	if !result.SourceUnchanged {
		cleanup()
		return result, fmt.Errorf("Import source changed during the copy; stop CPA and retry")
	}
	if err := os.Rename(tmp, absTarget); err != nil {
		cleanup()
		return result, err
	}
	result.Rows = sourceRows
	result.ImportedAt = now.UTC()
	return result, nil
}
