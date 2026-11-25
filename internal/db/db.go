package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ScanRecord represents the cached probe telemetry for a single AceStream ID.
type ScanRecord struct {
	Success          bool
	Time             float64
	Info             string
	Resolution       string
	Bitrate          string
	Peers            string
	LastUpdated      string
	MinutesAgo       int
	ConsecutiveFails int
}

// Store wraps SQLite persistence for stream quality scans.
type Store struct {
	mu   sync.Mutex
	conn *sql.DB
}

// Open initializes scans.db in dataDir and applies the schema.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = "."
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "scans.db")
	conn, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// Single writer connection keeps SQLite lock-free under concurrent goroutines.
	conn.SetMaxOpenConns(1)

	s := &Store{conn: conn}
	if err := s.init(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	_, err := s.conn.Exec(`
		CREATE TABLE IF NOT EXISTS scans (
			ace_id TEXT PRIMARY KEY,
			success BOOLEAN,
			time REAL,
			info TEXT,
			last_updated TIMESTAMP,
			consecutive_fails INTEGER DEFAULT 0
		)
	`)
	if err != nil {
		return err
	}
	// Migrate legacy databases created without consecutive_fails.
	_, _ = s.conn.Exec(`ALTER TABLE scans ADD COLUMN consecutive_fails INTEGER DEFAULT 0`)
	log.Println("SQLite Database initialized successfully.")
	return nil
}

// Close closes the underlying SQLite connection.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	return s.conn.Close()
}

// SaveScan upserts scan status for aceID, resetting or incrementing consecutive_fails.
func (s *Store) SaveScan(aceID string, success bool, elapsed float64, info string) {
	if aceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Format("2006-01-02 15:04:05")
	var query string
	if success {
		query = `
			INSERT INTO scans (ace_id, success, time, info, last_updated, consecutive_fails)
			VALUES (?, ?, ?, ?, ?, 0)
			ON CONFLICT(ace_id) DO UPDATE SET
				success=excluded.success,
				time=excluded.time,
				info=excluded.info,
				last_updated=excluded.last_updated,
				consecutive_fails=0
		`
	} else {
		query = `
			INSERT INTO scans (ace_id, success, time, info, last_updated, consecutive_fails)
			VALUES (?, ?, ?, ?, ?, 1)
			ON CONFLICT(ace_id) DO UPDATE SET
				success=excluded.success,
				time=excluded.time,
				info=excluded.info,
				last_updated=excluded.last_updated,
				consecutive_fails=consecutive_fails + 1
		`
	}

	if _, err := s.conn.Exec(query, aceID, success, elapsed, info, now); err != nil {
		log.Printf("Error saving scan to DB: %v", err)
	}
}

// GetAllScans returns all scan records keyed by ace_id.
func (s *Store) GetAllScans() map[string]ScanRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make(map[string]ScanRecord)
	rows, err := s.conn.Query(`SELECT ace_id, success, time, info, last_updated, COALESCE(consecutive_fails, 0) FROM scans`)
	if err != nil {
		log.Printf("Error loading scans from DB: %v", err)
		return result
	}
	defer rows.Close()

	now := time.Now()
	for rows.Next() {
		var (
			aceID    string
			success  sql.NullBool
			elapsed  sql.NullFloat64
			infoNull sql.NullString
			rawTS    any
			fails    int
		)
		if err := rows.Scan(&aceID, &success, &elapsed, &infoNull, &rawTS, &fails); err != nil {
			log.Printf("Error scanning row from DB: %v", err)
			continue
		}

		infoStr := infoNull.String
		res := "N/A"
		bitrate := "N/A"
		peers := "N/A"
		if infoStr != "" {
			parts := strings.Split(infoStr, "|")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			if len(parts) >= 3 {
				res = parts[0]
				bitrate = parts[1]
				peers = strings.TrimSpace(strings.ReplaceAll(parts[2], "Peers:", ""))
			} else {
				res = infoStr
			}
		}

		lastUpdatedStr, minutesAgo := parseTimestamp(rawTS, now)
		result[aceID] = ScanRecord{
			Success:          success.Bool,
			Time:             elapsed.Float64,
			Info:             infoStr,
			Resolution:       res,
			Bitrate:          bitrate,
			Peers:            peers,
			LastUpdated:      lastUpdatedStr,
			MinutesAgo:       minutesAgo,
			ConsecutiveFails: fails,
		}
	}
	return result
}

func parseTimestamp(raw any, now time.Time) (string, int) {
	if raw == nil {
		return "", 0
	}
	var tsStr string
	switch v := raw.(type) {
	case time.Time:
		tsStr = v.Format("2006-01-02 15:04:05")
		mins := int(now.Sub(v).Minutes())
		if mins < 0 {
			mins = 0
		}
		return tsStr, mins
	case []byte:
		tsStr = string(v)
	case string:
		tsStr = v
	default:
		tsStr = fmt.Sprint(v)
	}

	layouts := []string{"2006-01-02 15:04:05", time.RFC3339}
	for _, layout := range layouts {
		if parsed, err := time.ParseInLocation(layout, tsStr, time.Local); err == nil {
			mins := int(now.Sub(parsed).Minutes())
			if mins < 0 {
				mins = 0
			}
			return parsed.Format("2006-01-02 15:04:05"), mins
		}
	}
	return tsStr, 0
}
