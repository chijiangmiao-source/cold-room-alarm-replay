package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const maxBodyBytes = 1 << 20

type Server struct {
	store  *Store
	broker *Broker
	// 以下钩子仅用于测试，生产环境为 nil。
	// beforeReplay 在注册实时订阅之后、查询历史补发之前执行；
	// afterReplayQuery 在历史补发查询完成之后、进入实时循环之前执行。
	// 两者分别制造事件落入"补发/实时交叠窗口"两侧的场景。
	beforeReplay     func()
	afterReplayQuery func()
}

func main() {
	port := envOr("API_PORT", "8080")
	dbPath := envOr("DB_PATH", "/data/alarm.db")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	ctx := context.Background()
	store, err := OpenStore(ctx, "file:"+dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	srv := &Server{store: store, broker: NewBroker()}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/api/events", srv.handleEvents)
	mux.HandleFunc("/api/events/stream", srv.handleStream)

	addr := ":" + port
	log.Printf("alarm API listening on %s (db=%s)", addr, dbPath)
	if err := http.ListenAndServe(addr, withCommonHeaders(mux)); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// withCommonHeaders 为 SSE/前端跨域访问加上宽松 CORS，并为 SSE 关闭缓冲。
func withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	var in EventInput
	if err := dec.Decode(&in); err != nil {
		// 语法非法、body 过大等一律 400，且在写库之前返回，绝不占用序号。
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	// 拒绝在一个合法 JSON 值之后夹带额外数据（避免回调里混入私货）。
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid JSON body: unexpected trailing data")
		return
	}

	if err := validate(in); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, ve.Msg)
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ev, created, err := s.store.Insert(r.Context(), in)
	if err != nil {
		log.Printf("insert event %s: %v", in.EventID, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if created {
		w.Header().Set("X-Deduplicated", "false")
		w.WriteHeader(http.StatusCreated)
		// 只有首次接受的事件才进入实时推送；重复回调不再次广播。
		s.broker.Publish(ev)
	} else {
		// 幂等重放：返回既有记录，不新增、不分配新序号、不再次推送。
		w.Header().Set("X-Deduplicated", "true")
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(ev)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	var lastSeq int64 = 0
	if raw := r.URL.Query().Get("last_seq"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "last_seq must be a non-negative integer")
			return
		}
		lastSeq = v
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 1) 先注册实时订阅，再做历史补发：注册之后到达的事件绝不会只落在库中而错过通道。
	live, unsubscribe := s.broker.Subscribe()
	defer unsubscribe()

	ctx := r.Context()

	// 2) 历史补发：严格按服务端 seq 升序推送所有 seq > lastSeq 的事件。
	if s.beforeReplay != nil {
		s.beforeReplay()
	}
	sentSeq := lastSeq
	history, err := s.store.EventsAfter(ctx, lastSeq)
	if err != nil {
		log.Printf("replay after %d: %v", lastSeq, err)
		return
	}
	for _, ev := range history {
		if !writeSSEEvent(w, ev) {
			return
		}
		sentSeq = ev.Seq
	}
	flusher.Flush()

	if s.afterReplayQuery != nil {
		s.afterReplayQuery()
	}

	// 3) 进入实时推送。订阅窗口与补发窗口的交叠事件按 seq 去重：
	//    seq <= sentSeq 的一定已经在补发里发过，直接丢弃，保证不重复。
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	// 补发完成后先给一个边界标记，客户端据此知道“历史已齐”。
	if _, err := fmt.Fprintf(w, `event: replay-done`+"\ndata: {\"last_seq\":%d}\n\n", sentSeq); err != nil {
		return
	}
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-live:
			if !ok {
				// 本订阅因慢消费被 broker 踢出：结束连接，让客户端带最后序号重连补发。
				return
			}
			if ev.Seq <= sentSeq {
				continue // 交叠窗口内已补发
			}
			if !writeSSEEvent(w, ev) {
				return
			}
			sentSeq = ev.Seq
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w io.Writer, ev Event) bool {
	data, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: alarm\ndata: %s\n\n", data); err != nil {
		return false
	}
	return true
}

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: msg})
}
