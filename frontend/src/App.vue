<script setup>
import { onMounted, onBeforeUnmount, computed } from 'vue'
import { useAlarmStream } from './useAlarmStream.js'
import { useActiveDoors } from './useActiveDoors.js'

// 未关闭门快照：初始加载一次，之后收到实时告警逐条刷新，
// 断线补发完成（replay-done）再校准一次。快照失败只在门面板内提示。
const { doors: activeDoors, error: doorsError, refresh: refreshDoors } = useActiveDoors('')

const {
  events, connection, lastSeq, lastError, completeness, start, stop,
} = useAlarmStream('', {
  onLiveAlarm: () => refreshDoors(),
  onReplayDone: () => refreshDoors(),
})

onMounted(() => {
  start()
  refreshDoors()
})
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

    <div class="layout">
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

      <aside class="doors-panel" data-testid="active-doors">
        <div class="doors-head">
          <h2>未关闭门</h2>
          <span class="doors-count" data-testid="active-doors-count">{{ activeDoors.length }}</span>
        </div>

        <div v-if="doorsError" class="doors-error" data-testid="active-doors-error">
          <span>{{ doorsError }}</span>
          <button type="button" class="retry" data-testid="active-doors-retry" @click="refreshDoors">
            重试
          </button>
        </div>

        <div v-if="!doorsError && activeDoors.length === 0" class="doors-empty" data-testid="active-doors-empty">
          全部冷库门已关闭
        </div>

        <article
          v-for="d in activeDoors"
          :key="d.door_id"
          class="door-card"
          :class="kindMeta[d.latest_kind]?.cls"
          :data-door-id="d.door_id"
          data-testid="active-door"
        >
          <div class="door-card-head">
            <span class="door-name">{{ d.door_id }}</span>
            <span class="kind">{{ kindMeta[d.latest_kind]?.text ?? d.latest_kind }}</span>
          </div>
          <div class="door-card-body">
            <span>开始 #{{ d.start_seq }} · 最近 #{{ d.latest_seq }}</span>
            <span>设备时间：{{ fmtDeviceTime(d.occurred_at) }}</span>
          </div>
        </article>
      </aside>
    </div>
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
.layout {
  display: grid; grid-template-columns: minmax(0, 1fr) 300px;
  gap: 20px; align-items: start;
}
@media (max-width: 860px) {
  .layout { grid-template-columns: 1fr; }
}
.list { margin-top: 18px; display: flex; flex-direction: column; gap: 10px; }
.doors-panel {
  margin-top: 18px; border: 1px solid #243044; border-radius: 10px;
  background: #111927; padding: 14px 16px;
  position: sticky; top: 16px;
}
.doors-head { display: flex; justify-content: space-between; align-items: center; }
.doors-head h2 { font-size: 16px; margin: 0; }
.doors-count {
  min-width: 26px; text-align: center; padding: 2px 8px; border-radius: 999px;
  background: #7f1d1d; color: #fecaca; font-weight: 700; font-size: 13px;
  font-variant-numeric: tabular-nums;
}
.doors-error {
  margin-top: 12px; padding: 10px 12px; border: 1px solid #7f1d1d; border-radius: 8px;
  color: #fca5a5; font-size: 13px; display: flex; flex-direction: column; gap: 8px;
}
.doors-error .retry {
  align-self: flex-start; padding: 4px 14px; border-radius: 6px;
  border: 1px solid #7f1d1d; background: #1f2937; color: #fecaca;
  font-size: 13px; cursor: pointer;
}
.doors-error .retry:hover { background: #374151; }
.doors-empty { margin-top: 12px; color: #7184a0; font-size: 13px; }
.door-card {
  margin-top: 12px; border: 1px solid #243044; border-left-width: 4px;
  border-radius: 8px; padding: 10px 12px; background: #141c2b;
}
.door-card-head { display: flex; justify-content: space-between; align-items: baseline; gap: 8px; }
.door-name { font-weight: 700; color: #b8c6db; }
.door-card-body {
  margin-top: 6px; display: flex; flex-direction: column; gap: 2px;
  color: #8fa3bd; font-size: 12px; font-variant-numeric: tabular-nums;
}
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
