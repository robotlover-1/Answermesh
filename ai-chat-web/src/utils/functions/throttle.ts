// 流式渲染节流：把高频 progress 回调合并到固定时间窗内执行一次。
//
// 背景：前端每收到一个 delta 都要把「至今全文」重新 markdown 解析并整体替换 DOM
// （Message/Text.vue 的 computed + v-html），单帧成本随文本长度线性增长 → 全程 O(n²)，
// 长回答（尤其含代码块）会在主线程堆积成卡顿，表现为「出一段 → 卡一下 → 剩余内容一次全出」。
// （后端已改为只发增量、不再每帧发累积全文，但前端"重渲染全文"的成本仍随长度增长，节流仍需要。）
//
// 这里不改变最终结果，只限制刷 UI 的频率：窗口内的多次调用只保留最后一次参数，
// 并保证 trailing 一定执行（结束时 flush，避免丢掉最后一帧）。
export interface Throttled<A extends unknown[]> {
  (...args: A): void
  flush: () => void
}

export function throttleLast<A extends unknown[]>(
  fn: (...args: A) => void,
  wait = 50,
): Throttled<A> {
  let last = 0
  let timer: ReturnType<typeof setTimeout> | undefined
  let pending: A | undefined

  const invoke = (args: A) => {
    last = Date.now()
    pending = undefined
    fn(...args)
  }

  const throttled = ((...args: A) => {
    pending = args
    const remaining = wait - (Date.now() - last)

    if (remaining <= 0) {
      if (timer) { clearTimeout(timer); timer = undefined }
      invoke(args)
      return
    }
    // 窗口内已有排队的 trailing 时只覆盖最新参数（以最后一次为准），不重复挂定时器
    if (!timer) {
      timer = setTimeout(() => {
        timer = undefined
        if (pending)
          invoke(pending)
      }, remaining)
    }
  }) as Throttled<A>

  // 立即执行挂起的 trailing 并取消定时器；流程结束时调用，确保最后一帧不丢。
  throttled.flush = () => {
    if (timer) { clearTimeout(timer); timer = undefined }
    if (pending)
      invoke(pending)
  }

  return throttled
}
