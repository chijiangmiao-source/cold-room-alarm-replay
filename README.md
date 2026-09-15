# 冷库门告警接收与回放链路

食品冷库夜间频繁开门、中控浏览器偶尔断网。本项目解决两个痛点：

- **重复告警**：设备网关重试回调时，同一事件不会重复入库、重复弹窗；
- **漏告警**：中控断线期间到达的事件，重连后按服务端序号完整补发，边界时刻到达的事件也**不重不漏**。

链路：

```
设备网关 ──POST JSON──▶ Go API ──写入──▶ SQLite（严格递增、永不复用的服务端序号）
                          │
                          └──SSE──▶ Vue 中控页面（按序号显示，断线带最后序号重连）
```

## 快速开始（Docker Compose）

```bash
docker compose up -d --build     # 起 api 与 web
# 中控页面： http://localhost:${WEB_PORT:-8081}
# API：      http://localhost:${API_PORT:-8080}

docker compose run --rm verify   # 一次性端到端验收，通过后退出码 0
```

端口可用环境变量覆盖（容器内固定监听，宿主机映射随之改变）：

```bash
WEB_PORT=9000 API_PORT=9090 docker compose up -d --build
```

## 本地开发（不用 Docker）

```bash
# 终端 1：API（Go 1.23+，SQLite 为纯 Go 驱动，无需 cgo）
cd backend
go run .                       # 默认 :8080，DB 在 /data/alarm.db
DB_PATH=./alarm.db API_PORT=8080 go run .   # 本地常用写法

# 终端 2：前端
cd frontend
npm install
API_PORT=8080 npm run dev      # http://localhost:5173，/api 自动代理到 API

# 后端测试（存储 + SSE 续接，含竞态检测）
cd backend && go test -race ./...

# 端到端测试（需要先在 API_PORT 上跑着 API）
cd frontend && npx playwright install chromium
npx playwright test
```

---

## 接口

### 1. 设备回调：`POST /api/events`

设备网关提交一条 JSON，必填 `event_id`、`door_id`、`occurred_at`、`kind`：

| 字段 | 说明 |
| --- | --- |
| `event_id` | **全局唯一**。相同 id 的重复回调返回既有记录，不新增事件、不分配新序号（幂等） |
| `door_id` | 冷库门编号，必填 |
| `occurred_at` | 设备侧事件时间，RFC3339（如 `2026-09-14T22:00:00Z`）。**只用于展示，不参与排序** |
| `kind` | 只能是 `OPEN_TOO_LONG`、`FORCED_OPEN`、`CLOSED` |

首次接受返回 `201 Created`，响应头 `X-Deduplicated: false`：

```bash
curl -X POST http://localhost:8080/api/events \
  -H 'Content-Type: application/json' \
  -d '{
    "event_id": "gw-20260914-0001",
    "door_id": "door-A1",
    "kind": "OPEN_TOO_LONG",
    "occurred_at": "2026-09-14T22:03:10Z"
  }'
```

```json
{
  "seq": 1,
  "event_id": "gw-20260914-0001",
  "door_id": "door-A1",
  "kind": "OPEN_TOO_LONG",
  "occurred_at": "2026-09-14T22:03:10Z",
  "received_at": "2026-09-14T14:03:10.482911Z"
}
```

相同 `event_id` 重发（哪怕重试时其它字段变了）返回 `200 OK`、`X-Deduplicated: true`，
响应体仍是**第一次**那条记录，`seq` 不变：

```bash
curl -X POST http://localhost:8080/api/events \
  -H 'Content-Type: application/json' \
  -d '{"event_id":"gw-20260914-0001","door_id":"door-A1","kind":"FORCED_OPEN","occurred_at":"2026-09-14T22:03:10Z"}'
# HTTP 200, X-Deduplicated: true, 仍是 seq=1、kind=OPEN_TOO_LONG
```

非法 JSON、缺字段、非法 `kind`、非法时间一律 `400`，**不分配序号**：

```bash
curl -i -X POST http://localhost:8080/api/events -d '{not json'
# HTTP 400  {"error":"invalid JSON body: ..."}

curl -i -X POST http://localhost:8080/api/events \
  -d '{"event_id":"x","door_id":"d","kind":"OPEN","occurred_at":"2026-09-14T22:00:00Z"}'
# HTTP 400  {"error":"kind must be one of OPEN_TOO_LONG, FORCED_OPEN, CLOSED"}
```

### 服务端序号 `seq`

- 由 SQLite `INTEGER PRIMARY KEY AUTOINCREMENT` 分配，**严格递增、永不复用**，重启后继续增大；
- 页面排序、补发、去重只认 `seq`；设备时间 `occurred_at` 即使乱序也不影响顺序。

### 2. 事件流：`GET /api/events/stream`（SSE）

#### 重连方式（关键）

连接时带上**最后已显示序号** `last_seq`：

```
GET /api/events/stream?last_seq=42
```

服务端处理顺序：

1. 先注册实时订阅；
2. 再从 SQLite 按 `seq` 升序补发所有 `seq > last_seq` 的历史事件（`event: alarm`）；
3. 补发结束发一帧 `event: replay-done`；
4. 之后持续实时推送新事件，并每 15s 发一帧心跳注释。

因为「先订阅、后查库」，订阅与补发交叠窗口内到达的事件可能同时出现在两处，
服务端按 `seq` 去重（`seq <= 已补发序号` 的实时帧丢弃），**每条告警恰好下发一次**。
前端同样只接受 `seq > 本地最后序号` 的帧，双重保险。

首次打开页面用 `last_seq=0` 全量补发。EventSource 自带重连不会携带查询参数，
因此前端在 `onerror` 时主动关闭并带上最新 `last_seq` 重新建连（指数退避，上限 10s）。

帧示例：

```
event: alarm
data: {"seq":43,"event_id":"gw-...","door_id":"door-A1","kind":"FORCED_OPEN","occurred_at":"...","received_at":"..."}

event: replay-done
data: {"last_seq":43}
```

直接观察：

```bash
curl -N "http://localhost:8080/api/events/stream?last_seq=0"
```

### 3. 未关闭门快照：`GET /api/doors/active`（只读）

值班员接班时不必翻完整条时间线，直接看当前仍处于异常开启时段的门。
门时段由后端在**同一 SQLite 事务**内随事件落库维护：

- 首次接受的 `OPEN_TOO_LONG` / `FORCED_OPEN` 建立该门的异常时段；
- 同门后续告警只更新最近类型、最近序号与设备时间（开始序号不变）；
- `CLOSED` 结束该时段；
- 重复 `event_id` 的回调整体无效，时段不变；
- 升级后首次启动会从既有事件按序号重放，回算出一致的初始视图（只执行一次）。

```bash
curl http://localhost:8080/api/doors/active
```

```json
{
  "doors": [
    {
      "door_id": "door-A1",
      "start_seq": 3,
      "latest_seq": 7,
      "latest_kind": "FORCED_OPEN",
      "occurred_at": "2026-09-14T22:03:10Z"
    }
  ]
}
```

- 固定按异常开始序号 `start_seq` 升序排列；无未关闭门时返回 `{"doors":[]}`；
- `occurred_at` 是最近一次告警的设备时间，仅用于展示；
- 只接受 GET，其它方法返回 405；查询失败返回与事件提交一致的 `{"error":"..."}`。

### 4. 健康检查：`GET /healthz` → `200 {"status":"ok"}`

---

## 中控页面

- 顶部徽标显示连接状态：`连接中 / 补发断线期间事件 / 实时 / 已断开，自动重连中`；
- 每条告警一张卡片，展示服务端序号 `#seq`、类型、门号与**设备时间**；
- 时间线旁是「未关闭门」面板：数量徽标 + 每门的门号、最近告警类型、开始/最近序号与设备时间。
  收到实时告警后自动刷新，断线补发完成后再校准一次；快照加载失败只在面板内提示并可重试，
  不影响告警流与完整性判断；
- 「序号连续 · 屏幕完整」自检：若本地序号出现空洞（例如补发异常），会明确列出缺失序号，值班员可立刻判断屏幕是否完整。

## 验收服务 `verify`

`docker compose run --rm verify`（或本地 `go run ./cmd/verify http://localhost:8080`）
对运行中的 API 真实执行：

1. 非法 JSON / 缺字段 / 非法 `kind` 全部返回 400，且随后的合法事件序号紧接（非法请求不占号）；
2. 打开 SSE 后连续提交 3 条，实时收到、序号严格 +1；
3. 重复回调返回 200 + `X-Deduplicated: true` + 既有记录，且不再推送；
4. **主动断开 SSE**，断线期间连续提交并夹杂重复回调，再带最后序号重连，断言补发事件序号连续、每条恰好一次；
5. 补发完成后的新事件仍实时到达，序号紧接；
6. 乱序的设备时间不影响服务端顺序。

可重复执行：每次运行使用唯一 `event_id` 前缀，不依赖空库，也不污染结论。

## 目录结构

```
backend/
  main.go          HTTP 接口、校验、SSE 补发+实时续传、未关闭门快照
  store.go         SQLite 存储、幂等写入、按 seq 补发、门时段事务维护与回算
  broker.go        实时事件扇出（慢消费者踢除，强制其走补发恢复）
  store_test.go    存储/幂等/序号单调与重启不复用
  doors_test.go    门时段回算/生命周期/重复回调事务结果、快照接口固定顺序
  sse_test.go      SSE 补发、实时、断线续传、边界恰好一次
  cmd/verify/      一次性验收程序
frontend/
  src/             Vue3 页面、SSE 续传与未关闭门快照组合式函数
  e2e/             Playwright：回调 → 页面全链路（含断网窗口与门面板校准）
docker-compose.yml api / web / verify 三服务，WEB_PORT、API_PORT 可覆盖
```
