package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Event 是落库后对外暴露的告警记录。Seq 是服务端序号：
// 由 SQLite AUTOINCREMENT 分配，严格递增且永不复用，是排序与续传的唯一依据。
// OccurredAt 来自设备时钟，仅用于展示，绝不参与排序。
type Event struct {
	Seq        int64  `json:"seq"`
	EventID    string `json:"event_id"`
	DoorID     string `json:"door_id"`
	Kind       string `json:"kind"`
	OccurredAt string `json:"occurred_at"`
	ReceivedAt string `json:"received_at"`
}

// EventInput 是设备网关回调的请求体。
type EventInput struct {
	EventID    string `json:"event_id"`
	DoorID     string `json:"door_id"`
	OccurredAt string `json:"occurred_at"`
	Kind       string `json:"kind"`
}

// 允许的告警类型。
const (
	KindOpenTooLong = "OPEN_TOO_LONG"
	KindForcedOpen  = "FORCED_OPEN"
	KindClosed      = "CLOSED"
)

// ValidationError 表示请求内容非法，调用方应返回 4xx，且不得分配序号。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func validate(in EventInput) error {
	if in.EventID == "" {
		return &ValidationError{Msg: "event_id is required"}
	}
	if in.DoorID == "" {
		return &ValidationError{Msg: "door_id is required"}
	}
	if in.OccurredAt == "" {
		return &ValidationError{Msg: "occurred_at is required"}
	}
	if _, err := time.Parse(time.RFC3339, in.OccurredAt); err != nil {
		return &ValidationError{Msg: "occurred_at must be an RFC3339 timestamp, e.g. 2026-09-14T22:00:00Z"}
	}
	switch in.Kind {
	case KindOpenTooLong, KindForcedOpen, KindClosed:
	default:
		return &ValidationError{Msg: fmt.Sprintf("kind must be one of %s, %s, %s", KindOpenTooLong, KindForcedOpen, KindClosed)}
	}
	return nil
}

// Store 封装 SQLite。所有写入都通过 busy_timeout 容忍短暂锁竞争。
type Store struct {
	db *sql.DB
}

func OpenStore(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单连接即可串行化写入，避免 SQLITE_BUSY；吞吐对告警场景完全够用。
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS events (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id    TEXT NOT NULL UNIQUE,
	door_id     TEXT NOT NULL,
	kind        TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	received_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_seq ON events(seq);
`)
	return err
}

// ErrNotFound 用于按 event_id 查询未命中。
var ErrNotFound = errors.New("event not found")

// FindByEventID 返回既有记录。未命中时返回 ErrNotFound。
func (s *Store) FindByEventID(ctx context.Context, eventID string) (Event, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT seq, event_id, door_id, kind, occurred_at, received_at
FROM events WHERE event_id = ?`, eventID)
	return scanEvent(row)
}

// Insert 写入一条新告警并返回带服务端序号的完整记录。
// 返回 created=false 表示该 event_id 已存在（重复回调或唯一约束冲突），
// 此时返回的是既有记录，不新增行、不分配新序号。
func (s *Store) Insert(ctx context.Context, in EventInput) (ev Event, created bool, err error) {
	if existing, ferr := s.FindByEventID(ctx, in.EventID); ferr == nil {
		return existing, false, nil
	} else if !errors.Is(ferr, ErrNotFound) {
		return Event{}, false, ferr
	}

	received := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
INSERT INTO events (event_id, door_id, kind, occurred_at, received_at)
VALUES (?, ?, ?, ?, ?)`,
		in.EventID, in.DoorID, in.Kind, in.OccurredAt, received)
	if err != nil {
		// 唯一约束冲突：并发下的重复回调（单连接串行写入，正常走不到，仅作兜底）。
		if isUniqueConflict(err) {
			existing, ferr := s.FindByEventID(ctx, in.EventID)
			if ferr != nil {
				return Event{}, false, ferr
			}
			return existing, false, nil
		}
		return Event{}, false, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Event{}, false, err
	}
	return Event{
		Seq:        seq,
		EventID:    in.EventID,
		DoorID:     in.DoorID,
		Kind:       in.Kind,
		OccurredAt: in.OccurredAt,
		ReceivedAt: received,
	}, true, nil
}

// EventsAfter 返回序号严格大于 after 的全部事件，按序号升序——即补发顺序。
func (s *Store) EventsAfter(ctx context.Context, after int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, event_id, door_id, kind, occurred_at, received_at
FROM events WHERE seq > ? ORDER BY seq ASC`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// MaxSeq 返回当前最大序号，空库返回 0。
func (s *Store) MaxSeq(ctx context.Context) (int64, error) {
	var max sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM events`).Scan(&max); err != nil {
		return 0, err
	}
	return max.Int64, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(r rowScanner) (Event, error) {
	var ev Event
	err := r.Scan(&ev.Seq, &ev.EventID, &ev.DoorID, &ev.Kind, &ev.OccurredAt, &ev.ReceivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, err
	}
	return ev, nil
}

func isUniqueConflict(err error) bool {
	// modernc.org/sqlite 约束错误文本包含 "constraint failed"。
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}
