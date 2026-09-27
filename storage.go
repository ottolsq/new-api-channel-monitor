package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Storage 管理本地 SQLite 的测试记录。
type Storage struct {
	db *sql.DB
}

// NewStorage 打开 SQLite 数据库并初始化表结构。
// DSN 中启用 WAL 模式和 5 秒 busy timeout，减少并发读写时的 database is locked 错误。
func NewStorage(dbPath string) (*Storage, error) {
	dsn := makeSQLiteDSN(dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return &Storage{db: db}, nil
}

// makeSQLiteDSN 构造 SQLite DSN，附加 WAL 和 busy timeout 参数。
func makeSQLiteDSN(dbPath string) string {
	u := url.URL{
		Scheme: "file",
		Opaque: dbPath,
	}
	q := url.Values{}
	q.Set("_journal", "WAL")
	q.Set("_busy_timeout", "5000")
	u.RawQuery = q.Encode()
	return u.String()
}

// initSchema 创建 test_logs 表及索引。
func initSchema(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS test_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id INTEGER NOT NULL,
    channel_name TEXT NOT NULL,
    model TEXT NOT NULL,
    success INTEGER NOT NULL,
    latency_ms INTEGER,
    error_code TEXT,
    error_msg TEXT,
    alerted_at INTEGER DEFAULT 0,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_test_logs_channel_model_created
ON test_logs(channel_id, model, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_test_logs_created_at
ON test_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_test_logs_alert
ON test_logs(channel_id, model, success, alerted_at, created_at);
`
	_, err := db.Exec(schema)
	return err
}

// InsertLog 写入一条测试记录，返回自增 ID。
func (s *Storage) InsertLog(ctx context.Context, channelID int, channelName, model string, result TestResult, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
        INSERT INTO test_logs (channel_id, channel_name, model, success, latency_ms, error_code, error_msg, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
    `, channelID, channelName, model, boolToInt(result.Success), result.LatencyMs, result.ErrorCode, result.ErrorMsg, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("insert log: %w", err)
	}
	return res.LastInsertId()
}

// ShouldAlert 检查某渠道某模型在防抖窗口内是否已经告警过。
// debounceMinutes <= 0 表示不防抖，每次失败都允许告警。
func (s *Storage) ShouldAlert(ctx context.Context, channelID int, model string, debounceMinutes int, now time.Time) (bool, error) {
	if debounceMinutes <= 0 {
		return true, nil
	}
	cutoff := now.Add(-time.Duration(debounceMinutes) * time.Minute).Unix()
	var count int
	err := s.db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM test_logs
        WHERE channel_id = ? AND model = ? AND success = 0 AND alerted_at > ?
    `, channelID, model, cutoff).Scan(
		&count)
	if err != nil {
		return false, fmt.Errorf("check alert history: %w", err)
	}
	return count == 0, nil
}

// MarkAlertedByID 通过自增 ID 精确标记某条失败记录已告警。
func (s *Storage) MarkAlertedByID(ctx context.Context, logID int64) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE test_logs SET alerted_at = ?
        WHERE id = ?
    `, time.Now().Unix(), logID)
	if err != nil {
		return fmt.Errorf("mark alerted: %w", err)
	}
	return nil
}

// AvailabilitySummary 按渠道+模型聚合可用率。
type AvailabilitySummary struct {
	ChannelID           int64   `json:"channel_id"`
	ChannelName         string  `json:"channel_name"`
	Model               string  `json:"model"`
	Total               int64   `json:"total"`
	Success             int64   `json:"success"`
	AvgLatencyMs        float64 `json:"avg_latency_ms"`
	AvailabilityPercent float64 `json:"availability_percent"`
}

// QueryAvailability 查询截至 now 的最近 hours 小时可用率统计。
func (s *Storage) QueryAvailability(ctx context.Context, hours int, now time.Time) ([]AvailabilitySummary, error) {
	cutoff := now.Add(-time.Duration(hours) * time.Hour).Unix()
	rows, err := s.db.QueryContext(ctx, `
        SELECT
            channel_id,
            channel_name,
            model,
            COUNT(*) AS total,
            SUM(success) AS success_count,
            ROUND(AVG(CASE WHEN success = 1 THEN latency_ms ELSE NULL END), 0) AS avg_latency_ms,
            ROUND(CAST(SUM(success) AS REAL) / COUNT(*) * 100, 2) AS availability_percent
        FROM test_logs
        WHERE created_at >= ?
        GROUP BY channel_id, model
        ORDER BY channel_id, availability_percent ASC
    `, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query availability: %w", err)
	}
	defer rows.Close()

	var results []AvailabilitySummary
	for rows.Next() {
		var r AvailabilitySummary
		var success sql.NullInt64
		var avg sql.NullFloat64
		if err := rows.Scan(
			&r.ChannelID, &r.ChannelName, &r.Model, &r.Total, &success, &avg, &r.AvailabilityPercent); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		r.Success = success.Int64
		if avg.Valid {
			r.AvgLatencyMs = avg.Float64
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// QueryRecentFailures 查询截至 now 的最近 hours 小时内的失败明细。
func (s *Storage) QueryRecentFailures(ctx context.Context, hours int, now time.Time) ([]TestLogRecord, error) {
	cutoff := now.Add(-time.Duration(hours) * time.Hour).Unix()
	rows, err := s.db.QueryContext(ctx, `
        SELECT channel_id, channel_name, model, error_code, error_msg, created_at
        FROM test_logs
        WHERE success = 0 AND created_at >= ?
        ORDER BY created_at DESC
    `, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query failures: %w", err)
	}
	defer rows.Close()

	var results []TestLogRecord
	for rows.Next() {
		var r TestLogRecord
		var ts int64
		if err := rows.Scan(
			&r.ChannelID, &r.ChannelName, &r.Model, &r.ErrorCode, &r.ErrorMsg, &ts); err != nil {
			return nil, fmt.Errorf("scan failure: %w", err)
		}
		r.CreatedAt = time.Unix(ts, 0)
		results = append(results, r)
	}
	return results, rows.Err()
}

// TestLogRecord 单条失败记录。
type TestLogRecord struct {
	ChannelID   int       `json:"channel_id"`
	ChannelName string    `json:"channel_name"`
	Model       string    `json:"model"`
	ErrorCode   string    `json:"error_code"`
	ErrorMsg    string    `json:"error_msg"`
	CreatedAt   time.Time `json:"created_at"`
}

// Close 关闭数据库连接。
func (s *Storage) Close() error {
	return s.db.Close()
}

// boolToInt 将 bool 转为 0/1。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// splitModels 将逗号分隔的模型字符串拆分为切片。
func splitModels(models string) []string {
	parts := strings.Split(models, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
