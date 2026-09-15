// useActiveDoors 维护"未关闭门"快照：只读 GET /api/doors/active。
//
// 快照是派生视图，刷新时机由调用方决定（初始加载、实时告警、补发完成）。
// 加载失败只记录到 error，由门面板自行提示并允许重试——绝不影响告警流
// 与序号完整性判断。
import { ref } from 'vue'

export function useActiveDoors(baseUrl = '') {
  const doors = ref([])      // 未关闭门，按异常开始序号升序（服务端固定顺序）
  const error = ref('')      // 最近一次刷新失败的原因；成功即清空
  const loading = ref(false)

  async function refresh() {
    loading.value = true
    try {
      const res = await fetch(`${baseUrl}/api/doors/active`)
      const data = await res.json().catch(() => null)
      if (!res.ok) {
        // 服务端错误结构与事件提交一致：{"error": "..."}
        throw new Error(data?.error || `HTTP ${res.status}`)
      }
      doors.value = Array.isArray(data?.doors) ? data.doors : []
      error.value = ''
    } catch (err) {
      error.value = `未关闭门快照加载失败：${err.message}`
    } finally {
      loading.value = false
    }
  }

  return { doors, error, loading, refresh }
}
