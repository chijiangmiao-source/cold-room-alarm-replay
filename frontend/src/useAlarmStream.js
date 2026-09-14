// useAlarmStream 封装 SSE 连接与断线续传。
//
// 续传协议（与后端 README 一致）：
//   GET /api/events/stream?last_seq=<最后已显示序号>
//   服务端先按服务端序号升序补发所有 seq > last_seq 的历史事件（event: alarm），
//   补发结束发一帧 event: replay-done，之后进入实时推送。
// 前端只以 seq 为准：任何 seq <= lastSeq 的帧直接丢弃。这样无论补发与实时
// 在交叠窗口内如何到达，同一条告警都恰好渲染一次。

import { ref, computed } from 'vue'

export function useAlarmStream(baseUrl = '') {
  const events = ref([])           // 已显示事件，按 seq 升序
  const connection = ref('connecting') // connecting | replaying | live | disconnected
  const lastSeq = ref(0)
  const lastError = ref('')
  const replayDoneAt = ref(null)

  const seen = new Set()           // 防御性去重：seq -> true
  let es = null
  let reconnectTimer = null
  let reconnectDelay = 500
  let stopped = false

  function upsert(ev) {
    if (!Number.isInteger(ev.seq) || ev.seq <= 0) return
    // 协议边界：只接受比最后已显示序号更大的事件。
    if (ev.seq <= lastSeq.value || seen.has(ev.seq)) return
    seen.add(ev.seq)
    events.value.push(ev)
    events.value.sort((a, b) => a.seq - b.seq)
    lastSeq.value = ev.seq
  }

  function connect() {
    if (stopped) return
    connection.value = lastSeq.value === 0 ? 'connecting' : 'replaying'
    const url = `${baseUrl}/api/events/stream?last_seq=${lastSeq.value}`
    es = new EventSource(url)

    es.addEventListener('alarm', (e) => {
      try {
        upsert(JSON.parse(e.data))
      } catch (err) {
        lastError.value = `无法解析事件帧: ${err.message}`
      }
    })

    es.addEventListener('replay-done', () => {
      // 历史补发已全部到齐：之后到达的都是实时事件。
      replayDoneAt.value = new Date().toISOString()
      connection.value = 'live'
      reconnectDelay = 500
    })

    es.onopen = () => {
      // 空库或没有补发时后端不发 replay-done 也保持实时；
      // 状态在收到 replay-done 或第一条事件后修正。
      if (connection.value === 'connecting') connection.value = 'replaying'
    }

    es.onerror = () => {
      // EventSource 会自动重连，但它不会带自定义参数，也保证不了 last_seq 语义，
      // 因此显式关闭后由我们自己带最后序号重连。
      teardownES()
      connection.value = 'disconnected'
      scheduleReconnect()
    }
  }

  function teardownES() {
    if (es) {
      es.close()
      es = null
    }
  }

  function scheduleReconnect() {
    if (stopped || reconnectTimer) return
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null
      connect()
    }, reconnectDelay)
    // 指数退避，上限 10s，避免中控浏览器抖动时空转。
    reconnectDelay = Math.min(reconnectDelay * 2, 10_000)
  }

  function start() {
    stopped = false
    connect()
  }

  function stop() {
    stopped = true
    clearTimeout(reconnectTimer)
    teardownES()
  }

  // 序号连续性自检：最大序号减去事件数应等于去重前最小已见序号基线，
  // 值班员可一眼看出屏幕是否完整。
  const completeness = computed(() => {
    if (events.value.length === 0) return { complete: true, missing: [] }
    const present = new Set(events.value.map((e) => e.seq))
    const max = Math.max(...present)
    const missing = []
    for (let s = 1; s <= max; s++) {
      if (!present.has(s)) missing.push(s)
    }
    return { complete: missing.length === 0, missing }
  })

  return {
    events, connection, lastSeq, lastError, replayDoneAt, completeness,
    start, stop,
  }
}
