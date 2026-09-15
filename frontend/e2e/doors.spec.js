// @ts-check
import { test, expect, request as pwRequest } from '@playwright/test'

// 与 alarm.spec.js 相同：每次运行独立前缀，历史数据不影响断言。
// 两门使用专属 door_id，面板断言只针对本运行的门卡片。
const RUN = `doors-${Date.now()}`
const TS = '2026-09-14T22:00:00Z'
const DOOR_A = `${RUN}-A`
const DOOR_B = `${RUN}-B`

// 断线模拟：拦截 SSE 请求使其失败。注意不能用 ctx.setOffline——
// Chromium 的离线模拟不会断开已建立的 SSE 连接（事件仍会实时到达），
// 只有阻断流本身才能让页面进入真实的"断开-重连-补发"流程。
const STREAM_RE = /\/api\/events\/stream/

function body(eventId, doorId, kind) {
  return { event_id: eventId, door_id: doorId, kind, occurred_at: TS }
}

async function postEvent(api, eventId, doorId, kind) {
  const res = await api.post('/api/events', { data: body(eventId, doorId, kind) })
  return { status: res.status(), dedup: res.headers()['x-deduplicated'], ev: await res.json() }
}

test.describe.configure({ mode: 'serial' })

test.beforeAll(async ({ baseURL }) => {
  const api = await pwRequest.newContext({ baseURL })
  const res = await api.get('/healthz')
  expect(res.status()).toBe(200)
  await api.dispose()
})

test('两门告警、关闭一门、断线补发后，面板只保留另一门且时间线每条一次', async ({ page, request }) => {
  await page.goto('/')
  await expect(page.getByTestId('conn-badge')).toBeVisible()

  const panel = page.getByTestId('active-doors')
  const doorA = panel.locator(`[data-door-id="${DOOR_A}"]`)
  const doorB = panel.locator(`[data-door-id="${DOOR_B}"]`)

  // 1. 两门先后异常开启：面板各出现一张卡片，计数徽标与卡片数一致。
  const a1 = await postEvent(request, `${RUN}-a1`, DOOR_A, 'FORCED_OPEN')
  const b1 = await postEvent(request, `${RUN}-b1`, DOOR_B, 'OPEN_TOO_LONG')
  await expect(doorA, '门 A 应进入未关闭面板').toHaveCount(1, { timeout: 15_000 })
  await expect(doorB, '门 B 应进入未关闭面板').toHaveCount(1)
  await expect(doorA).toContainText(`开始 #${a1.ev.seq} · 最近 #${a1.ev.seq}`)
  await expect(doorA).toContainText('强制开门')
  await expect(doorB).toContainText('开门超时')
  const cardCount = await panel.locator('[data-testid="active-door"]').count()
  await expect(page.getByTestId('active-doors-count')).toHaveText(String(cardCount))

  // 2. 关闭门 A：面板只保留门 B。
  await postEvent(request, `${RUN}-a2`, DOOR_A, 'CLOSED')
  await expect(doorA, '门 A 关闭后应离开面板').toHaveCount(0, { timeout: 15_000 })
  await expect(doorB).toHaveCount(1)

  // 3. 模拟断线：拦截 SSE 后 reload，事件流无法建立，页面进入断开态。
  //    门快照接口不受影响，仍显示断线前的状态（最近序号为 b1）。
  await page.route(STREAM_RE, (route) => route.abort())
  await page.reload()
  await expect(page.getByTestId('conn-badge')).toHaveText('已断开，自动重连中', { timeout: 15_000 })
  await expect(doorB).toHaveCount(1)
  await expect(doorB).toContainText(`开始 #${b1.ev.seq} · 最近 #${b1.ev.seq}`)

  // 4. 断线期间：门 B 再报一条（最近类型/序号应随之更新），并夹杂重复回调。
  const b2 = await postEvent(request, `${RUN}-b2`, DOOR_B, 'FORCED_OPEN')
  const dup = await postEvent(request, `${RUN}-b1`, DOOR_B, 'CLOSED')
  expect(dup.dedup).toBe('true')
  expect(dup.ev.seq).toBe(b1.ev.seq)
  expect(dup.ev.kind).toBe('OPEN_TOO_LONG')

  // 5. 恢复事件流：页面自动重连并补发，replay-done 后校准门面板。
  await page.unroute(STREAM_RE)
  await expect(page.getByTestId('conn-badge')).toHaveText('实时', { timeout: 20_000 })
  await expect(doorA, '门 A 保持关闭，不回到面板').toHaveCount(0)
  await expect(doorB).toHaveCount(1)
  await expect(doorB).toContainText(`开始 #${b1.ev.seq} · 最近 #${b2.ev.seq}`, { timeout: 15_000 })
  await expect(doorB).toContainText('强制开门')

  // 6. 时间线每条恰好一次，序号连续性自检不受影响。
  for (const id of [`${RUN}-a1`, `${RUN}-b1`, `${RUN}-a2`, `${RUN}-b2`]) {
    await expect(page.locator(`article.event[data-event-id="${id}"]`)).toHaveCount(1)
  }
  await expect(page.getByTestId('completeness')).toContainText('序号连续')
  await expect(page.getByTestId('last-seq')).toContainText(`#${b2.ev.seq}`)
})

test('门快照加载失败只在面板内提示并可重试，告警流与完整性判断不受影响', async ({ page, request }) => {
  // 拦截快照接口，模拟加载失败。
  await page.route('**/api/doors/active', (route) =>
    route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"boom"}' }))
  await page.goto('/')

  const errorBox = page.getByTestId('active-doors-error')
  await expect(errorBox).toBeVisible()
  await expect(errorBox).toContainText('boom')

  // 快照失败期间，实时告警照常上屏、完整性判断照常。
  const id = `${RUN}-snap-1`
  const { ev } = await postEvent(request, id, DOOR_A, 'FORCED_OPEN')
  await expect(page.locator(`article.event[data-event-id="${id}"]`)).toHaveCount(1, { timeout: 15_000 })
  await expect(page.getByTestId('conn-badge')).toHaveText('实时')
  await expect(page.getByTestId('completeness')).toContainText('序号连续')

  // 接口恢复后点重试：错误消失，门卡片出现。
  await page.unroute('**/api/doors/active')
  await page.getByTestId('active-doors-retry').click()
  await expect(errorBox).toHaveCount(0)
  const doorA = page.getByTestId('active-doors').locator(`[data-door-id="${DOOR_A}"]`)
  await expect(doorA).toHaveCount(1)
  await expect(doorA).toContainText(`开始 #${ev.seq} · 最近 #${ev.seq}`)
})
