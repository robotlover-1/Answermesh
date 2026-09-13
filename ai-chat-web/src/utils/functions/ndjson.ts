// 读取「累积式」NDJSON 响应（axios onDownloadProgress 给的 xhr.responseText 是全量累积的）。
//
// 历史：最初每个 progress 事件取最后一个 '\n' 之后的内容直接 parse，落在帧中间就抛错，
// 被空 catch 吞掉 → 大量丢帧，表现为「出一段 → 卡住 → 剩余一次性全出」。
// 当时改成「每次只解析最后两个 '\n' 之间那一行」修好了它 —— 但那依赖一个**未写明的
// 隐含前提：一个 progress 事件里最多只有一个完整帧**。当年后端每帧发「累积全文」
// （几十 KB），浏览器交付时天然被切开，前提成立。
//
// 后端改为**只发增量**（ai-chat-backend chat.go，帧只有几个字）后，一个 progress
// 事件会一次装进好几帧，于是除最后一行外的帧全被静默丢弃：前端只收到零散的 delta，
// 累积成一串缺块的乱码（表现为回答内容错乱、残缺）。
//
// 现在：一次把 fresh 里**所有完整行**都交给 onFrame，末尾半行仍留给下次拼接。
export function createNdjsonReader<T>(onFrame: (frame: T) => void) {
  let offset = 0
  let carry = ''

  return (responseText: string) => {
    // 重新发起请求 / 文本被重置时，从头开始
    if (responseText.length < offset) {
      offset = 0
      carry = ''
    }
    // carry 是上次残留的半行，responseText.slice(offset) 是本次新增字节，拼起来才是完整流
    const fresh = carry + responseText.slice(offset)
    offset = responseText.length

    const lastNl = fresh.lastIndexOf('\n')
    if (lastNl === -1) {
      carry = fresh // 还没有完整行
      return
    }
    // 最后一个 '\n' 之前是本次可解析的全部完整行；之后是尚未结束的半行
    const complete = fresh.slice(0, lastNl)
    carry = fresh.slice(lastNl + 1)

    for (const line of complete.split('\n')) {
      const trimmed = line.trim()
      if (!trimmed)
        continue
      try {
        onFrame(JSON.parse(trimmed) as T)
      }
      catch {
        // 坏帧（理论上不再出现）忽略，不影响后续帧
      }
    }
  }
}
