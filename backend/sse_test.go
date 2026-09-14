package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerWith(t, nil)
}

func newTestServerWith(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	if srv == nil {
		store, err := OpenStore(context.Background(), "file:"+t.TempDir()+"/sse.db")
		if err != nil {
			t.Fatal(err)
		}
		srv = &Server{store: store, broker: NewBroker()}
	}
	ts := httptest.NewServer(withCommonHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/events":
			srv.handleEvents(w, r)
		case "/api/events/stream":
			srv.handleStream(w, r)
		default:
			http.NotFound(w, r)
		}
	})))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { srv.store.Close() })
	return ts
}

func postEvent(t *testing.T, ts *httptest.Server, body string) (int, Event, http.Header) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/events", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var ev Event
	_ = json.Unmarshal(raw, &ev)
	return resp.StatusCode, ev, resp.Header
}

func eventBody(id, kind string) string {
	return fmt.Sprintf(
		`{"event_id":%q,"door_id":"d1","kind":%q,"occurred_at":"2026-09-14T22:00:00Z"}`, id, kind)
}

// sseClient 打开一个带 last_seq 的 SSE 连接，逐条解析 alarm 帧。
type sseClient struct {
	t     *testing.T
	resp  *http.Response
	sc    *bufio.Scanner
	alarm chan Event
	done  chan struct{}
}

func openSSE(t *testing.T, ts *httptest.Server, lastSeq int64) *sseClient {
	t.Helper()
	url := fmt.Sprintf("%s/api/events/stream?last_seq=%d", ts.URL, lastSeq)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("stream status %d: %s", resp.StatusCode, body)
	}
	c := &sseClient{
		t: t, resp: resp, sc: bufio.NewScanner(resp.Body),
		alarm: make(chan Event, 64), done: make(chan struct{}),
	}
	c.sc.Buffer(make([]byte, 1<<20), 1<<20)
	go c.readLoop()
	return c
}

func (c *sseClient) readLoop() {
	defer close(c.done)
	// 按 SSE 规范逐帧组装：空行分隔帧，帧内 event: 行决定类型，
	// 只把 event: alarm 的 data 解析为事件（忽略 replay-done 与注释心跳）。
	var eventType, data string
	flushFrame := func() {
		if eventType == "alarm" {
			var ev Event
			if err := json.Unmarshal([]byte(data), &ev); err == nil && ev.Seq > 0 {
				c.alarm <- ev
			}
		}
		eventType, data = "", ""
	}
	for c.sc.Scan() {
		line := c.sc.Text()
		switch {
		case line == "":
			flushFrame()
		case strings.HasPrefix(line, ":"):
			// 注释帧（心跳），忽略。
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

func (c *sseClient) next(timeout time.Duration) (Event, bool) {
	select {
	case ev := <-c.alarm:
		return ev, true
	case <-time.After(timeout):
		return Event{}, false
	}
}

func (c *sseClient) close_() { c.resp.Body.Close() }

func waitSeqs(t *testing.T, c *sseClient, want []int64) {
	t.Helper()
	for _, w := range want {
		ev, ok := c.next(3 * time.Second)
		if !ok {
			t.Fatalf("timeout waiting for seq %d (wanted sequence %v)", w, want)
		}
		if ev.Seq != w {
			t.Fatalf("got seq %d, want %d (wanted sequence %v)", ev.Seq, w, want)
		}
	}
}

func TestSSEReplayThenLive(t *testing.T) {
	ts := newTestServer(t)

	// 历史 1..2。
	postEvent(t, ts, eventBody("h1", "CLOSED"))
	postEvent(t, ts, eventBody("h2", "OPEN_TOO_LONG"))

	c := openSSE(t, ts, 0)
	defer c.close_()
	waitSeqs(t, c, []int64{1, 2})

	// 补发完后到达的事件走实时通道。
	postEvent(t, ts, eventBody("live1", "FORCED_OPEN"))
	waitSeqs(t, c, []int64{3})
}

func TestSSEResumeAfterDisconnectIsExactlyOnce(t *testing.T) {
	ts := newTestServer(t)

	// 在线看到 seq 1、2 后断开。
	c := openSSE(t, ts, 0)
	postEvent(t, ts, eventBody("a", "CLOSED"))
	postEvent(t, ts, eventBody("b", "CLOSED"))
	waitSeqs(t, c, []int64{1, 2})
	c.close_()

	// 断线期间：连续提交新事件，且夹杂重复回调。
	postEvent(t, ts, eventBody("c", "FORCED_OPEN"))
	postEvent(t, ts, eventBody("d", "OPEN_TOO_LONG"))
	code, dup, hdr := postEvent(t, ts, eventBody("c", "CLOSED")) // 与首次字段不同也必须返回既有记录
	if code != http.StatusOK || hdr.Get("X-Deduplicated") != "true" || dup.Seq != 3 || dup.Kind != "FORCED_OPEN" {
		t.Fatalf("duplicate callback not idempotent: code=%d dedup=%s seq=%d kind=%s",
			code, hdr.Get("X-Deduplicated"), dup.Seq, dup.Kind)
	}
	postEvent(t, ts, eventBody("e", "CLOSED"))

	// 带最后已显示序号 2 重连：必须只收到 3、4、5，每条恰好一次。
	c2 := openSSE(t, ts, 2)
	defer c2.close_()
	waitSeqs(t, c2, []int64{3, 4, 5})

	// 后续实时事件序号紧接 6，证明重复回调没有占号。
	postEvent(t, ts, eventBody("f", "CLOSED"))
	waitSeqs(t, c2, []int64{6})

	// 再等一拍确认没有重复推送。
	if _, ok := c2.next(400 * time.Millisecond); ok {
		t.Fatal("unexpected duplicate event after catch-up")
	}
}

func TestSSEBoundaryArrivalsNotLostOrDuplicated(t *testing.T) {
	store, err := OpenStore(context.Background(), "file:"+t.TempDir()+"/boundary.db")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: store, broker: NewBroker()}
	ts := newTestServerWith(t, srv)

	// 侧 A：事件在"实时订阅已注册、历史补发查询尚未执行"时提交。
	// 它既会被推上实时通道、也会出现在补发结果里，去重逻辑必须保证只下发一次。
	t.Run("between_subscribe_and_replay_query", func(t *testing.T) {
		srv.beforeReplay = func() { postEvent(t, ts, eventBody("edge-before", "CLOSED")) }
		defer func() { srv.beforeReplay = nil }()

		c := openSSE(t, ts, 0)
		defer c.close_()
		waitSeqs(t, c, []int64{1})
		if ev, ok := c.next(300 * time.Millisecond); ok {
			t.Fatalf("boundary event duplicated: got seq %d twice", ev.Seq)
		}
	})

	// 侧 B：事件在"历史补发查询完成之后、进入实时循环之前"提交。
	// 它不在补发结果里，只能走实时通道，同样必须恰好到达一次。
	t.Run("between_replay_query_and_live_loop", func(t *testing.T) {
		postEvent(t, ts, eventBody("history-2", "CLOSED"))                                    // seq 2
		srv.afterReplayQuery = func() { postEvent(t, ts, eventBody("edge-after", "CLOSED")) } // seq 3
		defer func() { srv.afterReplayQuery = nil }()

		c := openSSE(t, ts, 1) // 补发 seq 2，边界 seq 3 走实时
		defer c.close_()
		waitSeqs(t, c, []int64{2, 3})
		if ev, ok := c.next(300 * time.Millisecond); ok {
			t.Fatalf("boundary event duplicated: got extra seq %d", ev.Seq)
		}
	})
}

// 无钩子竞态压测：多轮"刚建立连接就连续写入"，无论事件走补发还是实时，
// 合并结果都必须是连续序号、每条一次。
func TestSSERacyCatchUpStress(t *testing.T) {
	ts := newTestServer(t)
	const rounds, perRound = 6, 5
	var expected int64
	for r := 0; r < rounds; r++ {
		c := openSSE(t, ts, expected)
		for i := 0; i < perRound; i++ {
			postEvent(t, ts, eventBody(fmt.Sprintf("race-%d-%d", r, i), "CLOSED"))
		}
		want := make([]int64, perRound)
		for i := range want {
			expected++
			want[i] = expected
		}
		waitSeqs(t, c, want)
		if ev, ok := c.next(300 * time.Millisecond); ok {
			t.Fatalf("round %d: duplicated seq %d", r, ev.Seq)
		}
		c.close_()
	}
}

func TestInvalidCallbacksRejectedWithoutSeq(t *testing.T) {
	ts := newTestServer(t)

	badBodies := []string{
		`{not json`,
		`{"event_id":"","door_id":"d","kind":"CLOSED","occurred_at":"2026-09-14T22:00:00Z"}`,
		`{"event_id":"x","door_id":"","kind":"CLOSED","occurred_at":"2026-09-14T22:00:00Z"}`,
		`{"event_id":"x","door_id":"d","kind":"CLOSED"}`,
		`{"event_id":"x","door_id":"d","kind":"WRONG","occurred_at":"2026-09-14T22:00:00Z"}`,
		`{"event_id":"x","door_id":"d","kind":"CLOSED","occurred_at":"midnight"}`,
	}
	for _, body := range badBodies {
		code, _, _ := postEvent(t, ts, body)
		if code != http.StatusBadRequest {
			t.Fatalf("body %s: want 400, got %d", body, code)
		}
	}

	// 全部被拒之后，第一条合法事件必须是 seq 1——非法请求不占号。
	code, ev, _ := postEvent(t, ts, eventBody("first", "CLOSED"))
	if code != http.StatusCreated || ev.Seq != 1 {
		t.Fatalf("want 201 seq=1, got %d seq=%d", code, ev.Seq)
	}
}

func TestResumeFromMiddleAndLiveDoesNotReplayTwice(t *testing.T) {
	ts := newTestServer(t)
	for i := 1; i <= 4; i++ {
		postEvent(t, ts, eventBody(fmt.Sprintf("e%d", i), "CLOSED"))
	}
	// 已显示到 3，重连只补 4，再实时收 5，交叠窗口内不重复。
	c := openSSE(t, ts, 3)
	defer c.close_()
	waitSeqs(t, c, []int64{4})
	postEvent(t, ts, eventBody("e5", "CLOSED"))
	waitSeqs(t, c, []int64{5})
	if _, ok := c.next(300 * time.Millisecond); ok {
		t.Fatal("boundary overlap produced a duplicated event")
	}
}
