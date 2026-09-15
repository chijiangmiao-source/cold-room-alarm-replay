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

// ActiveDoor 是"未关闭门"视图的一行：该门当前处于异常开启时段。
// StartSeq 是建立时段的首条告警序号，LatestSeq/LatestKind/OccurredAt
// 来自同时段内最近一次告警（OccurredAt 为设备时间，仅用于展示）。
type ActiveDoor struct {
	DoorID     string `json:"door_id"`
	StartSeq   int64  `json:"start_seq"`
	LatestSeq  int64  `json:"latest_seq"`
	LatestKind string `json:"latest_kind"`
	OccurredAt string `json:"occurred_at"`
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
CREATE TABLE IF NOT EXISTS door_states (
	door_id     TEXT PRIMARY KEY,
	start_seq   INTEGER NOT NULL,
	latest_seq  INTEGER NOT NULL,
	latest_kind TEXT NOT NULL,
	occurred_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`)
	if err != nil {
		return err
	}
	return s.backfillDoorStates(ctx)
}

// backfillDoorStates 从既有 events 按 seq 升序重放出门异常时段，结果与实时维护完全一致。
// 只在升级后的首次启动执行一次（meta 表标记），整个重放在单事务内完成：
// 要么全部门时段一次建齐，要么不留下半截状态。
func (s *Store) backfillDoorStates(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var marker string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'door_states_backfilled'`).Scan(&marker)
	switch {
	case err == nil:
		return nil // 已回算过，空转
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}

	// 先把历史事件全部读出（单连接下避免边查边写），再在事务内逐条应用。
	rows, err := tx.QueryContext(ctx, `SELECT seq, door_id, kind, occurred_at FROM events ORDER BY seq ASC`)
	if err != nil {
		return err
	}
	var history []Event
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.Seq, &ev.DoorID, &ev.Kind, &ev.OccurredAt); err != nil {
			rows.Close()
			return err
		}
		history = append(history, ev)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, ev := range history {
		if err := applyDoorState(ctx, tx, ev); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('door_states_backfilled', '1')`); err != nil {
		return err
	}
	return tx.Commit()
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
// 事件行与门异常时段在同一 SQLite 事务内落库：要么都生效，要么都不生效。
// 返回 created=false 表示该 event_id 已存在（重复回调或唯一约束冲突），
// 此时返回的是既有记录，不新增行、不分配新序号，门状态也保持不变。
func (s *Store) Insert(ctx context.Context, in EventInput) (ev Event, created bool, err error) {
	if existing, ferr := s.FindByEventID(ctx, in.EventID); ferr == nil {
		return existing, false, nil
	} else if !errors.Is(ferr, ErrNotFound) {
		return Event{}, false, ferr
	}

	received := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, false, err
	}
	// 提交后再次 Rollback 会返回 ErrTxDone，安全忽略。
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
INSERT INTO events (event_id, door_id, kind, occurred_at, received_at)
VALUES (?, ?, ?, ?, ?)`,
		in.EventID, in.DoorID, in.Kind, in.OccurredAt, received)
	if err != nil {
		// 唯一约束冲突：并发下的重复回调（单连接串行写入，正常走不到，仅作兜底）。
		if isUniqueConflict(err) {
			// 先显式回滚释放单连接，再查既有记录，否则查询会等不到连接。
			tx.Rollback()
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
	ev = Event{
		Seq:        seq,
		EventID:    in.EventID,
		DoorID:     in.DoorID,
		Kind:       in.Kind,
		OccurredAt: in.OccurredAt,
		ReceivedAt: received,
	}
	if err := applyDoorState(ctx, tx, ev); err != nil {
		return Event{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Event{}, false, err
	}
	return ev, true, nil
}

// applyDoorState 在事务内把一条首次接受的事件应用到门异常时段：
// 首次 OPEN_TOO_LONG / FORCED_OPEN 建立时段；同门后续告警只更新最近类型、
// 最近序号与设备时间（start_seq 保持首次建立时的值）；CLOSED 结束时段。
// 重复 event_id 在 Insert 查重阶段就已返回，永远不会走到这里。
func applyDoorState(ctx context.Context, tx *sql.Tx, ev Event) error {
	switch ev.Kind {
	case KindOpenTooLong, KindForcedOpen:
		_, err := tx.ExecContext(ctx, `
INSERT INTO door_states (door_id, start_seq, latest_seq, latest_kind, occurred_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(door_id) DO UPDATE SET
	latest_seq  = excluded.latest_seq,
	latest_kind = excluded.latest_kind,
	occurred_at = excluded.occurred_at`,
			ev.DoorID, ev.Seq, ev.Seq, ev.Kind, ev.OccurredAt)
		return err
	case KindClosed:
		_, err := tx.ExecContext(ctx, `DELETE FROM door_states WHERE door_id = ?`, ev.DoorID)
		return err
	default:
		return nil
	}
}

// ActiveDoors 返回当前所有未关闭门，按异常开始序号升序——接班时最该先看的顺序。
// 空结果返回空切片而非 nil，保证 JSON 序列化为 []。
func (s *Store) ActiveDoors(ctx context.Context) ([]ActiveDoor, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT door_id, start_seq, latest_seq, latest_kind, occurred_at
FROM door_states ORDER BY start_seq ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActiveDoor{}
	for rows.Next() {
		var d ActiveDoor
		if err := rows.Scan(&d.DoorID, &d.StartSeq, &d.LatestSeq, &d.LatestKind, &d.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
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
