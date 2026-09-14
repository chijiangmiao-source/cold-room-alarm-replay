// verify 是 docker compose 中的一次性验收服务：对运行中的 API 做真实联调，
// 验证"回调接收 → 幂等去重 → 序号严格递增 → SSE 断线续传恰好一次"整条链路。
// 成功退出码 0，任何断言失败退出码非 0。
//
// 用法：verify [API_BASE_URL]（默认 http://api:8080，本地可用 http://localhost:8080）
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type event struct {
	Seq        int64  `json:"seq"`
	EventID    string `json:"event_id"`
	DoorID     string `json:"door_id"`
	Kind       string `json:"kind"`
	OccurredAt string `json:"occurred_at"`
}

type failures []string

func (f *failures) check(cond bool, msg string, args ...any) {
	if !cond {
		*f = append(*f, "✗ "+fmt.Sprintf(msg, args...))
	}
}

func main() {
	base := "http://api:8080"
	if len(os.Args) > 1 && os.Args[1] != "" {
		base = os.Args[1]
	}
	if v := os.Getenv("API_BASE_URL"); v != "" {
		base = v
	}
	client := &http.Client{Timeout: 10 * time.Second}

	if err := waitHealthy(client, base); err != nil {
		fmt.Printf("verify: API 不可用: %v\n", err)
		os.Exit(1)
	}

	var fails failures
	run := fmt.Sprintf("verify-%d", time.Now().UnixNano())
	const doorID = "verify-door"
	mk := func(id, kind, at string) event {
		return event{EventID: run + "-" + id, DoorID: doorID, Kind: kind, OccurredAt: at}
	}

	// ---- 1. 先发一条基线事件，拿到当前真实最大序号（不依赖空库假设） ----------
	baseline := mustPost(client, &fails, base, mk("baseline", "CLOSED", "2026-09-14T22:00:00Z"))

	// ---- 2. 非法请求必须全部 400，且不得占用序号 ----------------------------
	postExpect(client, &fails, base, http.StatusBadRequest, `{not json`, "非法 JSON")
	postExpect(client, &fails, base, http.StatusBadRequest,
		fmt.Sprintf(`{"event_id":"%s-x","door_id":"%s","kind":"CLOSED"}`, run, doorID), "缺 occurred_at")
	postExpect(client, &fails, base, http.StatusBadRequest,
		fmt.Sprintf(`{"event_id":"%s-x","door_id":"%s","kind":"BOGUS","occurred_at":"2026-09-14T22:00:00Z"}`, run, doorID), "非法 kind")
	postExpect(client, &fails, base, http.StatusBadRequest,
		fmt.Sprintf(`{"event_id":"","door_id":"%s","kind":"CLOSED","occurred_at":"2026-09-14T22:00:00Z"}`, doorID), "缺 event_id")

	// ---- 3. 打开 SSE（last_seq=基线序号），覆盖"连接已建立后事件到达" --------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	got := make(chan event, 64)
	go stream(ctx, &fails, base, baseline.Seq, ready, got)
	<-ready

	// ---- 4. 连续提交 3 条，序号必须严格 +1（非法请求没有占号） --------------
	var live []event
	for i, kind := range []string{"OPEN_TOO_LONG", "FORCED_OPEN", "CLOSED"} {
		ev, code, dedup := postJSON(client, &fails, base,
			mk(fmt.Sprintf("live-%d", i), kind, fmt.Sprintf("2026-09-14T22:0%d:00Z", i+1)))
		fails.check(code == http.StatusCreated, "新事件 live-%d 应返回 201，实际 %d", i, code)
		fails.check(dedup == "false", "新事件 live-%d 的 X-Deduplicated 应为 false", i)
		live = append(live, ev)
	}
	fails.check(live[0].Seq == baseline.Seq+1,
		"非法请求不得占号：期望首条 seq=%d，实际 %d", baseline.Seq+1, live[0].Seq)
	for i := 1; i < len(live); i++ {
		fails.check(live[i].Seq == live[i-1].Seq+1,
			"连续提交序号必须 +1：%d 后应为 %d，实际 %d", live[i-1].Seq, live[i-1].Seq+1, live[i].Seq)
	}
	collect(&fails, got, live, "实时")

	// ---- 5. 重复回调：返回既有记录、200、不占号、不再推送 --------------------
	dup, code, dedup := postJSON(client, &fails, base,
		mk("live-0", "CLOSED", "2026-09-14T09:00:00Z")) // 同 event_id，其它字段故意不同
	fails.check(code == http.StatusOK, "重复回调应返回 200，实际 %d", code)
	fails.check(dedup == "true", "重复回调 X-Deduplicated 应为 true")
	fails.check(dup.Seq == live[0].Seq && dup.Kind == live[0].Kind && dup.DoorID == live[0].DoorID,
		"重复回调必须返回既有记录：got seq=%d kind=%s door=%s，want seq=%d kind=%s door=%s",
		dup.Seq, dup.Kind, dup.DoorID, live[0].Seq, live[0].Kind, live[0].DoorID)
	select {
	case ev := <-got:
		fails.check(false, "重复回调不得再次推送，却收到 seq=%d", ev.Seq)
	case <-time.After(500 * time.Millisecond):
	}

	// ---- 6. 模拟中控断网：断开 SSE，断线期间连续提交并夹杂重复回调 -----------
	cancel()
	time.Sleep(200 * time.Millisecond)
	gap := []event{
		mustPost(client, &fails, base, mk("gap-0", "CLOSED", "2026-09-14T22:10:00Z")),
		mustPost(client, &fails, base, mk("gap-1", "FORCED_OPEN", "2026-09-14T22:11:00Z")),
	}
	dup2, code, _ := postJSON(client, &fails, base, mk("live-2", "OPEN_TOO_LONG", "2026-09-14T22:03:00Z"))
	fails.check(code == http.StatusOK && dup2.Seq == live[2].Seq,
		"断线窗口内重复回调仍须幂等：code=%d seq=%d", code, dup2.Seq)
	fails.check(gap[0].Seq == live[2].Seq+1 && gap[1].Seq == live[2].Seq+2,
		"断线期间序号必须连续：期望 %d,%d 实际 %d,%d",
		live[2].Seq+1, live[2].Seq+2, gap[0].Seq, gap[1].Seq)

	// ---- 7. 带最后已显示序号重连：先补发，且每条恰好一次 --------------------
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	got2 := make(chan event, 64)
	go stream(ctx2, &fails, base, live[2].Seq, make(chan struct{}), got2)
	collect(&fails, got2, gap, "断线补发")

	// 补发完成后到达的事件仍走实时通道且序号紧接。
	after := mustPost(client, &fails, base, mk("after-resume", "CLOSED", "2026-09-14T22:12:00Z"))
	fails.check(after.Seq == gap[1].Seq+1, "重连后实时事件序号应紧接：want %d got %d", gap[1].Seq+1, after.Seq)
	collect(&fails, got2, []event{after}, "补发后实时")

	// ---- 8. 设备时间只用于展示，乱序设备时间不影响服务端顺序 ----------------
	cl1 := mustPost(client, &fails, base, mk("clock-late", "CLOSED", "2026-01-01T00:00:00Z"))
	cl2 := mustPost(client, &fails, base, mk("clock-early", "CLOSED", "2026-12-31T00:00:00Z"))
	fails.check(cl2.Seq == cl1.Seq+1, "排序只看服务端 seq，与设备时间无关：%d -> %d", cl1.Seq, cl2.Seq)

	if len(fails) == 0 {
		fmt.Println("verify: 全部通过 ✔  回调幂等、序号连续不复用、断线补发与实时推送均恰好一次")
		os.Exit(0)
	}
	for _, m := range fails {
		fmt.Println(m)
	}
	fmt.Printf("verify: %d 项失败\n", len(fails))
	os.Exit(1)
}

func waitHealthy(client *http.Client, base string) error {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && bytes.Contains(body, []byte("ok")) {
				return nil
			}
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%w（最后错误: %v）", errors.New("healthz 在 60s 内未通过"), lastErr)
}

func postExpect(client *http.Client, fails *failures, base string, want int, body, label string) {
	resp, err := client.Post(base+"/api/events", "application/json", strings.NewReader(body))
	if err != nil {
		fails.check(false, "%s 请求失败: %v", label, err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	fails.check(resp.StatusCode == want, "%s：期望状态 %d，实际 %d", label, want, resp.StatusCode)
}

func postJSON(client *http.Client, fails *failures, base string, in event) (event, int, string) {
	raw, _ := json.Marshal(in)
	resp, err := client.Post(base+"/api/events", "application/json", bytes.NewReader(raw))
	if err != nil {
		fails.check(false, "POST %s 失败: %v", in.EventID, err)
		return event{}, 0, ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out event
	_ = json.Unmarshal(data, &out)
	return out, resp.StatusCode, resp.Header.Get("X-Deduplicated")
}

func mustPost(client *http.Client, fails *failures, base string, in event) event {
	ev, code, dedup := postJSON(client, fails, base, in)
	fails.check(code == http.StatusCreated && dedup == "false",
		"事件 %s 应为新建 201，实际 %d", in.EventID, code)
	return ev
}

// collect 从 SSE 通道收集与 want 等长的事件，校验 seq/event_id 一一对应且无重复。
// 不属于本次期望集合的事件（其它客户端或历史残留）忽略。
func collect(fails *failures, ch <-chan event, want []event, label string) {
	wantSet := make(map[int64]string, len(want))
	for _, w := range want {
		wantSet[w.Seq] = w.EventID
	}
	gotSet := make(map[int64]bool)
	deadline := time.After(8 * time.Second)
	for len(gotSet) < len(wantSet) {
		select {
		case ev := <-ch:
			if id, ok := wantSet[ev.Seq]; ok {
				fails.check(ev.EventID == id, "%s：seq=%d 的 event_id 不符：%s != %s", label, ev.Seq, ev.EventID, id)
				fails.check(!gotSet[ev.Seq], "%s：seq=%d 被重复推送", label, ev.Seq)
				gotSet[ev.Seq] = true
			}
		case <-deadline:
			fails.check(false, "%s：超时只收到 %d/%d 条（缺 seq: %v）", label, len(gotSet), len(wantSet), missing(wantSet, gotSet))
			return
		}
	}
	// 再观察一拍，确认期望集合内没有重复帧。
	time.Sleep(400 * time.Millisecond)
	for {
		select {
		case ev := <-ch:
			if _, ok := wantSet[ev.Seq]; ok {
				fails.check(!gotSet[ev.Seq], "%s：收尾后 seq=%d 又重复到达", label, ev.Seq)
			}
		default:
			return
		}
	}
}

func missing(want map[int64]string, got map[int64]bool) []int64 {
	var out []int64
	for seq := range want {
		if !got[seq] {
			out = append(out, seq)
		}
	}
	return out
}

// stream 打开 SSE 连接并解析 alarm 帧。ready 在响应头到达（订阅已注册）时关闭。
func stream(ctx context.Context, fails *failures, base string, lastSeq int64, ready chan struct{}, out chan<- event) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/events/stream?last_seq=%d", base, lastSeq), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// ctx 取消导致的错误是预期内的主动断连，不算失败。
		if ctx.Err() == nil {
			fails.check(false, "SSE 连接失败: %v", err)
		}
		return
	}
	defer resp.Body.Close()
	if ready != nil {
		close(ready)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			continue
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return
		}
	}
}
