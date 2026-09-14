<script setup>
import { onMounted, onBeforeUnmount, computed } from 'vue'
import { useAlarmStream } from './useAlarmStream.js'

const {
  events, connection, lastSeq, lastError, completeness, start, stop,
} = useAlarmStream('')

onMounted(start)
onBeforeUnmount(stop)

const connectionMeta = {
  connecting: { text: '连接中…', cls: 'badge-connecting' },
  replaying: { text: '补发断线期间事件…', cls: 'badge-replaying' },
  live: { text: '实时', cls: 'badge-live' },
  disconnected: { text: '已断开，自动重连中', cls: 'badge-disconnected' },
}
const badge = computed(() => connectionMeta[connection.value] ?? connectionMeta.connecting)

const kindMeta = {
  OPEN_TOO_LONG: { text: '开门超时', cls: 'kind-long' },
  FORCED_OPEN: { text: '强制开门', cls: 'kind-forced' },
  CLOSED: { text: '已关闭', cls: 'kind-closed' },
}

function fmtDeviceTime(s) {
  // 设备时间仅用于展示。
  const d = new Date(s)
  return Number.isNaN(d.getTime()) ? s : d.toLocaleString('zh-CN', { hour12: false })
}

const latestFirst = computed(() => [...events.value].sort((a, b) => b.seq - a.seq))
</script>

<template>
  <main class="screen">
    <header class="topbar">
      <h1>冷库门告警中控</h1>
      <div class="status">
        <span class="badge" :class="badge.cls" data-testid="conn-badge">{{ badge.text }}</span>
        <span class="seq-info" data-testid="last-seq">最后序号 #{{ lastSeq }}</span>
        <span
          class="badge"
          :class="completeness.complete ? 'badge-live' : 'badge-disconnected'"
          data-testid="completeness"
        >
          {{ completeness.complete ? '序号连续 · 屏幕完整' : `缺序号: ${completeness.missing.join(', ')}` }}
        </span>
      </div>
    </header>

    <p v-if="lastError" class="error" data-testid="error">{{ lastError }}</p>

    <section class="list">
      <div v-if="latestFirst.length === 0" class="empty" data-testid="empty">
        暂无告警，等待设备网关上报…
      </div>
      <article
        v-for="ev in latestFirst"
        :key="ev.seq"
        class="event"
        :class="kindMeta[ev.kind]?.cls"
        :data-seq="ev.seq"
        :data-event-id="ev.event_id"
      >
        <div class="event-head">
          <span class="seq">#{{ ev.seq }}</span>
          <span class="kind" data-testid="kind">{{ kindMeta[ev.kind]?.text ?? ev.kind }}</span>
          <span class="door" data-testid="door">{{ ev.door_id }}</span>
        </div>
        <div class="event-body">
          <span>设备时间：{{ fmtDeviceTime(ev.occurred_at) }}</span>
          <span class="event-id">event_id: {{ ev.event_id }}</span>
        </div>
      </article>
    </section>
  </main>
</template>

<style>
:root {
  color-scheme: dark;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  font-family: -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif;
  background: #0e1420;
  color: #e6edf3;
}
.screen { max-width: 980px; margin: 0 auto; padding: 24px; }
.topbar {
  display: flex; justify-content: space-between; align-items: center;
  flex-wrap: wrap; gap: 12px; border-bottom: 1px solid #243044; padding-bottom: 16px;
}
h1 { font-size: 22px; margin: 0; }
.status { display: flex; gap: 10px; align-items: center; }
.badge {
  padding: 4px 12px; border-radius: 999px; font-size: 13px; font-weight: 600;
}
.badge-connecting { background: #33415c; color: #c6d2e1; }
.badge-replaying { background: #7c5e10; color: #ffe9a8; }
.badge-live { background: #14532d; color: #b7f7c8; }
.badge-disconnected { background: #7f1d1d; color: #fecaca; animation: blink 1s infinite; }
@keyframes blink { 50% { opacity: 0.55; } }
.seq-info { font-variant-numeric: tabular-nums; color: #93a4bb; font-size: 13px; }
.error { color: #fca5a5; margin: 12px 0; }
.empty { color: #7184a0; text-align: center; padding: 60px 0; }
.list { margin-top: 18px; display: flex; flex-direction: column; gap: 10px; }
.event {
  border: 1px solid #243044; border-left-width: 4px; border-radius: 10px;
  padding: 12px 16px; background: #141c2b;
}
.event-head { display: flex; gap: 14px; align-items: baseline; }
.seq { font-weight: 700; color: #7dd3fc; font-variant-numeric: tabular-nums; }
.kind { font-weight: 700; }
.door { color: #b8c6db; font-size: 14px; }
.event-body {
  margin-top: 6px; display: flex; justify-content: space-between;
  color: #8fa3bd; font-size: 13px; flex-wrap: wrap; gap: 6px;
}
.event-id { font-family: ui-monospace, monospace; }
.kind-long { border-left-color: #f59e0b; }
.kind-forced { border-left-color: #ef4444; }
.kind-closed { border-left-color: #22c55e; }
.kind-long .kind { color: #fbbf24; }
.kind-forced .kind { color: #f87171; }
.kind-closed .kind { color: #4ade80; }
</style>
