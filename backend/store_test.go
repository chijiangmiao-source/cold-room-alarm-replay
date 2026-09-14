package main

import (
	"context"
	"fmt"
	"testing"
)

func validInput(id string) EventInput {
	return EventInput{
		EventID:    id,
		DoorID:     "door-1",
		Kind:       KindOpenTooLong,
		OccurredAt: "2026-09-14T22:00:00Z",
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*EventInput)
		want string
	}{
		{"missing event_id", func(in *EventInput) { in.EventID = "" }, "event_id"},
		{"missing door_id", func(in *EventInput) { in.DoorID = "" }, "door_id"},
		{"missing occurred_at", func(in *EventInput) { in.OccurredAt = "" }, "occurred_at"},
		{"bad occurred_at", func(in *EventInput) { in.OccurredAt = "last night" }, "RFC3339"},
		{"bad kind", func(in *EventInput) { in.Kind = "OPEN" }, "kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput("e-1")
			tc.mut(&in)
			err := validate(in)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if _, ok := err.(*ValidationError); !ok {
				t.Fatalf("want *ValidationError, got %T", err)
			}
		})
	}

	for _, kind := range []string{KindOpenTooLong, KindForcedOpen, KindClosed} {
		in := validInput("e-kind-" + kind)
		in.Kind = kind
		if err := validate(in); err != nil {
			t.Fatalf("kind %s should be valid: %v", kind, err)
		}
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(context.Background(), "file:"+t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestInsertAssignsMonotonicSeq(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	var prev int64
	for i := 0; i < 5; i++ {
		in := validInput("evt-" + string(rune('a'+i)))
		// 故意让设备时间乱序，证明排序只看服务端 seq。
		in.OccurredAt = fmt.Sprintf("2026-09-14T20:%02d:00Z", 59-i)
		ev, created, err := s.Insert(ctx, in)
		if err != nil || !created {
			t.Fatalf("insert: %v created=%v", err, created)
		}
		if ev.Seq <= prev {
			t.Fatalf("seq not strictly increasing: got %d after %d", ev.Seq, prev)
		}
		prev = ev.Seq
	}
}

func TestDuplicateEventIDReturnsExisting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	first, created, err := s.Insert(ctx, validInput("dup-1"))
	if err != nil || !created {
		t.Fatalf("first insert: %v created=%v", err, created)
	}

	// 相同 event_id 的重复回调，哪怕其他字段不同，也必须返回既有记录。
	in2 := validInput("dup-1")
	in2.Kind = KindForcedOpen
	in2.DoorID = "door-other"
	second, created, err := s.Insert(ctx, in2)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if created {
		t.Fatal("duplicate callback must not create a new row")
	}
	if second.Seq != first.Seq || second.Kind != first.Kind || second.DoorID != first.DoorID {
		t.Fatalf("duplicate returned a different record: %+v vs %+v", second, first)
	}

	// 非法请求先于一切：被拒事件不占用序号。
	bad := validInput("bad")
	bad.Kind = "NOPE"
	if err := validate(bad); err == nil {
		t.Fatal("bad input should fail validation")
	}
	good, created, _ := s.Insert(ctx, validInput("good"))
	if !created || good.Seq != first.Seq+1 {
		t.Fatalf("rejected event must not consume a seq: got %d, want %d", good.Seq, first.Seq+1)
	}
}

func TestEventsAfterReplay(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if _, _, err := s.Insert(ctx, validInput(id)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.EventsAfter(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 3 {
		t.Fatalf("replay mismatch: %+v", got)
	}

	got, _ = s.EventsAfter(ctx, 3)
	if len(got) != 0 {
		t.Fatalf("nothing should follow seq 3, got %+v", got)
	}
}

func TestSeqNeverReusedAcrossReopen(t *testing.T) {
	dir := t.TempDir() + "/persist.db"
	ctx := context.Background()

	s1, err := OpenStore(ctx, "file:"+dir)
	if err != nil {
		t.Fatal(err)
	}
	ev, _, _ := s1.Insert(ctx, validInput("before-restart"))
	s1.Close()

	s2, err := OpenStore(ctx, "file:"+dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	ev2, created, _ := s2.Insert(ctx, validInput("after-restart"))
	if !created || ev2.Seq != ev.Seq+1 {
		t.Fatalf("seq must keep increasing after restart: %d then %d", ev.Seq, ev2.Seq)
	}
}
