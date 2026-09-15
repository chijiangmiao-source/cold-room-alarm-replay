package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// doorInput 构造指定门的事件请求体。
func doorInput(id, doorID, kind string) EventInput {
	return EventInput{
		EventID:    id,
		DoorID:     doorID,
		Kind:       kind,
		OccurredAt: "2026-09-14T22:00:00Z",
	}
}

func mustInsert(t *testing.T, s *Store, in EventInput) Event {
	t.Helper()
	ev, created, err := s.Insert(context.Background(), in)
	if err != nil || !created {
		t.Fatalf("insert %s: err=%v created=%v", in.EventID, err, created)
	}
	return ev
}

func mustActiveDoors(t *testing.T, s *Store) []ActiveDoor {
	t.Helper()
	doors, err := s.ActiveDoors(context.Background())
	if err != nil {
		t.Fatalf("active doors: %v", err)
	}
	return doors
}

// seedLegacyDB 模拟升级前的旧库：只有 events 表（无 door_states/meta），
// 直接写入历史事件，供 OpenStore 迁移时回算"未关闭门"视图。
func seedLegacyDB(t *testing.T, path string, evs []EventInput) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE events (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id    TEXT NOT NULL UNIQUE,
	door_id     TEXT NOT NULL,
	kind        TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	received_at TEXT NOT NULL
)`); err != nil {
		t.Fatal(err)
	}
	for i, in := range evs {
		if _, err := db.Exec(`
INSERT INTO events (event_id, door_id, kind, occurred_at, received_at)
VALUES (?, ?, ?, ?, ?)`,
			in.EventID, in.DoorID, in.Kind, in.OccurredAt,
			fmt.Sprintf("2026-09-14T22:0%d:00Z", i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackfillDoorStatesFromHistory(t *testing.T) {
	path := t.TempDir() + "/legacy.db"
	// 历史：A 开后关；B 先强制开门、后又开门超时（同门两条）；C 强制开门后未关。
	seedLegacyDB(t, path, []EventInput{
		doorInput("h1", "door-A", KindOpenTooLong),
		doorInput("h2", "door-B", KindForcedOpen),
		doorInput("h3", "door-A", KindClosed),
		doorInput("h4", "door-B", KindOpenTooLong),
		doorInput("h5", "door-C", KindForcedOpen),
	})

	s, err := OpenStore(context.Background(), "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	doors := mustActiveDoors(t, s)
	if len(doors) != 2 {
		t.Fatalf("want 2 active doors, got %+v", doors)
	}
	// 按异常开始序号升序：B(start=2) 在 C(start=5) 前。
	b, c := doors[0], doors[1]
	if b.DoorID != "door-B" || b.StartSeq != 2 || b.LatestSeq != 4 || b.LatestKind != KindOpenTooLong {
		t.Fatalf("door-B backfill mismatch: %+v", b)
	}
	if b.OccurredAt != "2026-09-14T22:00:00Z" {
		t.Fatalf("door-B occurred_at mismatch: %+v", b)
	}
	if c.DoorID != "door-C" || c.StartSeq != 5 || c.LatestSeq != 5 || c.LatestKind != KindForcedOpen {
		t.Fatalf("door-C backfill mismatch: %+v", c)
	}

	// 回算只发生一次：重启后视图保持，且新事件继续在同一事务里维护。
	s.Close()
	s2, err := OpenStore(context.Background(), "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	mustInsert(t, s2, doorInput("n1", "door-B", KindClosed))
	doors = mustActiveDoors(t, s2)
	if len(doors) != 1 || doors[0].DoorID != "door-C" {
		t.Fatalf("after reopen+close: %+v", doors)
	}
}

func TestDoorStateLifecycleSameDoor(t *testing.T) {
	s := newTestStore(t)

	// 同门多次告警：首次建立时段，后续只更新最近类型与序号。
	e1 := mustInsert(t, s, doorInput("d1-open", "door-1", KindOpenTooLong))
	e2 := mustInsert(t, s, doorInput("d1-forced", "door-1", KindForcedOpen))
	doors := mustActiveDoors(t, s)
	if len(doors) != 1 {
		t.Fatalf("want 1 active door, got %+v", doors)
	}
	d := doors[0]
	if d.StartSeq != e1.Seq || d.LatestSeq != e2.Seq || d.LatestKind != KindForcedOpen {
		t.Fatalf("same-door updates mismatch: %+v (e1=%d e2=%d)", d, e1.Seq, e2.Seq)
	}

	// CLOSED 结束时段。
	mustInsert(t, s, doorInput("d1-close", "door-1", KindClosed))
	if doors := mustActiveDoors(t, s); len(doors) != 0 {
		t.Fatalf("CLOSED must end the period, got %+v", doors)
	}

	// 关闭后再次开启：建立新时段，start_seq 取新告警序号。
	e4 := mustInsert(t, s, doorInput("d1-reopen", "door-1", KindForcedOpen))
	doors = mustActiveDoors(t, s)
	if len(doors) != 1 || doors[0].StartSeq != e4.Seq || doors[0].LatestSeq != e4.Seq {
		t.Fatalf("reopen must start a new period: %+v", doors)
	}
}

func TestDuplicateCallbackKeepsDoorState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustInsert(t, s, doorInput("dup-open", "door-1", KindOpenTooLong))
	e2 := mustInsert(t, s, doorInput("dup-forced", "door-1", KindForcedOpen))

	// 重复回调（哪怕 kind 是 CLOSED）必须整体无效：事件、序号、门状态都不变。
	dup := doorInput("dup-open", "door-1", KindClosed)
	ev, created, err := s.Insert(ctx, dup)
	if err != nil || created {
		t.Fatalf("duplicate insert: err=%v created=%v", err, created)
	}
	if ev.Kind != KindOpenTooLong {
		t.Fatalf("duplicate must return the original record, got %+v", ev)
	}
	doors := mustActiveDoors(t, s)
	if len(doors) != 1 || doors[0].LatestSeq != e2.Seq || doors[0].LatestKind != KindForcedOpen {
		t.Fatalf("duplicate callback changed door state: %+v", doors)
	}

	// 重复一条已接受的 CLOSED 也不能把已重新开启的门关掉。
	mustInsert(t, s, doorInput("dup-close", "door-1", KindClosed))
	e4 := mustInsert(t, s, doorInput("dup-reopen", "door-1", KindOpenTooLong))
	if _, created, err := s.Insert(ctx, doorInput("dup-close", "door-1", KindClosed)); err != nil || created {
		t.Fatalf("duplicate CLOSED: err=%v created=%v", err, created)
	}
	doors = mustActiveDoors(t, s)
	if len(doors) != 1 || doors[0].LatestSeq != e4.Seq {
		t.Fatalf("duplicate CLOSED must not end the new period: %+v", doors)
	}
}

// ---- HTTP 接口：GET /api/doors/active ----

func getActiveDoors(t *testing.T, tsURL string) (int, []ActiveDoor) {
	t.Helper()
	resp, err := http.Get(tsURL + "/api/doors/active")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Doors []ActiveDoor `json:"doors"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode doors: %v (body=%s)", err, raw)
	}
	return resp.StatusCode, body.Doors
}

func doorEventBody(id, doorID, kind string) string {
	return fmt.Sprintf(
		`{"event_id":%q,"door_id":%q,"kind":%q,"occurred_at":"2026-09-14T22:00:00Z"}`, id, doorID, kind)
}

func TestActiveDoorsAPI(t *testing.T) {
	ts := newTestServer(t)

	// 空库：200 且 doors 是空数组而非 null。
	code, doors := getActiveDoors(t, ts.URL)
	if code != http.StatusOK || doors == nil || len(doors) != 0 {
		t.Fatalf("empty: code=%d doors=%+v", code, doors)
	}

	// 只读接口：POST 一律 405，错误结构与事件提交一致。
	resp, err := http.Post(ts.URL+"/api/doors/active", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST want 405, got %d", resp.StatusCode)
	}
	var errBody map[string]string
	if err := json.Unmarshal(raw, &errBody); err != nil || errBody["error"] == "" {
		t.Fatalf("405 body must use the shared error structure, got %s", raw)
	}

	// 乱序门号提交，返回顺序必须按异常开始序号固定。
	postEvent(t, ts, doorEventBody("api-1", "door-Z", "FORCED_OPEN"))   // seq 1
	postEvent(t, ts, doorEventBody("api-2", "door-A", "OPEN_TOO_LONG")) // seq 2
	postEvent(t, ts, doorEventBody("api-3", "door-Z", "OPEN_TOO_LONG")) // seq 3，同门更新
	_, doors = getActiveDoors(t, ts.URL)
	if len(doors) != 2 || doors[0].DoorID != "door-Z" || doors[1].DoorID != "door-A" {
		t.Fatalf("order must follow start_seq, got %+v", doors)
	}
	if doors[0].StartSeq != 1 || doors[0].LatestSeq != 3 || doors[0].LatestKind != "OPEN_TOO_LONG" {
		t.Fatalf("door-Z mismatch: %+v", doors[0])
	}

	// 重复回调不改变视图。
	code, dup, hdr := postEvent(t, ts, doorEventBody("api-1", "door-Z", "CLOSED"))
	if code != http.StatusOK || hdr.Get("X-Deduplicated") != "true" || dup.Kind != "FORCED_OPEN" {
		t.Fatalf("duplicate not idempotent: code=%d dedup=%s kind=%s", code, hdr.Get("X-Deduplicated"), dup.Kind)
	}
	_, doors = getActiveDoors(t, ts.URL)
	if len(doors) != 2 || doors[0].LatestSeq != 3 {
		t.Fatalf("duplicate callback changed the view: %+v", doors)
	}

	// CLOSED 结束时段，另一门不受影响。
	postEvent(t, ts, doorEventBody("api-4", "door-Z", "CLOSED"))
	_, doors = getActiveDoors(t, ts.URL)
	if len(doors) != 1 || doors[0].DoorID != "door-A" || doors[0].StartSeq != 2 || doors[0].LatestSeq != 2 {
		t.Fatalf("after close: %+v", doors)
	}
}
