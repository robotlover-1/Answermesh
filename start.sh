#!/usr/bin/env bash
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

# 必须用普通用户运行：sudo 会把 $HOME 切到 /root，$PATH 里依赖的 $HOME/.local/bin
# （pip --user 装的 nuxt/go 工具）就找不到；且服务均为高端口、无特权需求。
if [ "$(id -u)" -eq 0 ]; then
  echo "✘ 请勿用 sudo/root 运行 start.sh（会找不到用户级 python 包 nuxt）。请先 sudo ./stop.sh 清理，再以普通用户 ./start.sh" >&2
  exit 1
fi

REBUILD=0
[[ "${1:-}" == "--rebuild" ]] && REBUILD=1

export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
mkdir -p "$LOG_DIR" "$PID_DIR"

# deploy/app/.env 若存在就先载入本进程环境 —— 它同时供两处使用：
#   ① 本脚本自己（DEEPSEEK_API_KEY 检查、以及传给 start_one 拉起的服务，proxy 只在启动时读它）
#   ② frpc 的渲染（ensure_frpc 里会再按需解析一次，两条路径不冲突）
# 有了这一步，「.env 里填一次，之后 ./start.sh 裸跑」才成立；否则 FRP_* 能用而
# DEEPSEEK_API_KEY 到不了 proxy，表现为聊天一直转圈、proxy 直连返回 401。
load_env_file "$FRPC_ENV"

echo "== 环境/补编译 =="

# ---- zrpc v2：libzrpc.a 增量构建 ----
# 依赖：third_party/zrpc/src|include 的 .c/.h，或 zrpc-go 的 .c/.h 更新时，重建静态库；
# 之后三个 Go 服务必须重编（它们的 go.mod replace 到 ../zrpc-go，而 zrpc-go 链接 libzrpc.a）。
ZRPC_LIB="$BASE/third_party/zrpc/build/libzrpc.a"
ZRPC_SVC_BINS=("$BASE/bin/ai-chat-service" "$BASE/bin/keywords-filter" "$BASE/bin/ai-chat-backend")
zrpc_need=0
[ $REBUILD -eq 1 ] && zrpc_need=1
if [ $zrpc_need -eq 0 ]; then
  if [ ! -f "$ZRPC_LIB" ]; then
    zrpc_need=1
  else
    if find "$BASE/third_party/zrpc/src" "$BASE/third_party/zrpc/include" "$BASE/third_party/zrpc/ntyco" "$BASE/zrpc-go" \
        \( -name '*.c' -o -name '*.h' \) -newer "$ZRPC_LIB" -print -quit 2>/dev/null | grep -q .; then
      zrpc_need=1
    fi
  fi
fi
if [ $zrpc_need -eq 1 ]; then
  echo "  [build] libzrpc (make)"
  if [ $REBUILD -eq 1 ]; then ( cd "$BASE/third_party/zrpc" && make clean >/dev/null 2>&1 || true ); fi
  if ! ( cd "$BASE/third_party/zrpc" && make ); then
    echo "  [build] libzrpc ✘ 编译失败"
    build_failed+=("libzrpc")
  fi
  # C/zrpc-go 更新会改变静态库 → 强制三个服务重编（即使它们自己的 *.go 没变）
  for tgt in "${ZRPC_SVC_BINS[@]}"; do rm -f "$tgt"; done
fi

# 目标二进制缺失 / 存在更新的 *.go / --rebuild 时重建。格式: 名|构建目录|包|输出(相对 BASE)
GO_BUILDS=(
  "ai-chat-backend|$BASE/ai-chat-backend|./cmd/|bin/ai-chat-backend"
  "ai-chat-service|$BASE/ai-chat-service/chat-server|.|bin/ai-chat-service"
  "keywords-filter|$BASE/keywords-filter/filter-server|.|bin/keywords-filter"
  "mock-openai-api|$BASE/mock-openai-api|.|bin/mock-openai-api"
  "openai-api-proxy|$BASE/openai-api-proxy|.|bin/openai-api-proxy"
)

build_failed=()
for b in "${GO_BUILDS[@]}"; do
  IFS='|' read -r _name _dir _pkg _out <<< "$b"
  target="$BASE/$_out"
  need=0
  [ $REBUILD -eq 1 ] && need=1
  [ -f "$target" ] || need=1
  find "$_dir" -name '*.go' -newer "$target" -print -quit 2>/dev/null | grep -q . && need=1
  if [ $need -eq 1 ]; then
    echo "  [build] $_name"
    if ! ( cd "$_dir" && go build -o "$target" "$_pkg" ); then
      echo "  [build] $_name ✘ 编译失败"
      build_failed+=("$_name")
    fi
  fi
done

# 前端：dist/www 缺失、或**源文件比产物新**（git pull 换了 src 却没重建）→ 重建。
# 只判"文件是否存在"是不够的：git pull 更新了 src/ 时 dist 仍在，会被跳过，
# 于是浏览器继续跑旧 JS —— 若这次改动同时涉及前后端协议（如流式帧字段），
# 旧前端配新后端会直接渲染不出内容。Go 服务那边一直有 -newer 判断，前端此前漏了。
frontend_need=0
[ $REBUILD -eq 1 ] && frontend_need=1
[ -f "$BASE/ai-chat-web/dist/index.html" ] || frontend_need=1
[ -f "$BASE/ai-chat-backend/www/index.html" ] || frontend_need=1
if [ $frontend_need -eq 0 ]; then
  if find "$BASE/ai-chat-web/src" "$BASE/ai-chat-web/public" -type f \
       -newer "$BASE/ai-chat-web/dist/index.html" -print -quit 2>/dev/null | grep -q .; then
    frontend_need=1
  fi
  for _fe in index.html vite.config.ts package.json; do
    if [ -f "$BASE/ai-chat-web/$_fe" ] && [ "$BASE/ai-chat-web/$_fe" -nt "$BASE/ai-chat-web/dist/index.html" ]; then
      frontend_need=1; break
    fi
  done
  # backend/www 是 dist 的拷贝产物，落后也要重跑一遍（否则后端仍发旧页面）
  if [ "$BASE/ai-chat-web/dist/index.html" -nt "$BASE/ai-chat-backend/www/index.html" ]; then
    frontend_need=1
  fi
fi
if [ $frontend_need -eq 1 ]; then
  echo "  [build] 前端 (pnpm install + build-only)"
  if ! ( cd "$BASE/ai-chat-web" && pnpm install --fetch-retries=15 && pnpm build-only ); then
    echo "  [build] 前端 ✘ 编译失败"
    build_failed+=("前端")
  else
    mkdir -p "$BASE/ai-chat-backend/www"
    cp -r "$BASE/ai-chat-web/dist/." "$BASE/ai-chat-backend/www/"
  fi
fi

# kvstore(C, 子模块)：二进制缺失 / 存在更新的 .c/.h / --rebuild 时 make（新 clone 无二进制需自动补）
KV_TARGET="$BASE/kvstore/kvstore/kvstore"
kv_need=0
[ $REBUILD -eq 1 ] && kv_need=1
[ -f "$KV_TARGET" ] || kv_need=1
if [ $kv_need -eq 0 ]; then
  find "$BASE/kvstore/kvstore" \( -name '*.c' -o -name '*.h' \) -newer "$KV_TARGET" -print -quit 2>/dev/null | grep -q . && kv_need=1
fi
if [ $kv_need -eq 1 ]; then
  echo "  [build] kvstore (make)"
  if ! ( cd "$BASE/kvstore/kvstore" && make ); then
    echo "  [build] kvstore ✘ 编译失败（需 make/gcc，见 kvstore/README）"
    build_failed+=("kvstore")
  fi
fi

if [ ${#build_failed[@]} -ne 0 ]; then
  echo
  echo "✘ 编译失败: ${build_failed[*]}; 请修复后重试 ./start.sh"
  exit 1
fi

if [ ! -f "$BASE/semantic/models/e5s-v1/model.onnx" ]; then
  if [ "${ECHO_FETCH_MODEL:-}" = "1" ]; then
    echo "  [model] 未找到模型，自动从 GitHub Release 下载（ECHO_FETCH_MODEL=1）..."
    if ! bash "$BASE/semantic/tools/fetch_model.sh"; then
      echo "  [model] ✘ 模型下载/校验失败，见 semantic/tools/fetch_model.sh"
    fi
  fi
  if [ ! -f "$BASE/semantic/models/e5s-v1/model.onnx" ]; then
    echo "⚠ 未找到 semantic 模型文件（semantic/models/e5s-v1/model.onnx）：语义缓存将不可用（/embed 会 500 → 优雅 miss，聊天不受影响）。"
    echo "  获取：bash semantic/tools/fetch_model.sh   # 从 GitHub Release 下载+sha256 校验（约 79MB）"
    echo "       或手动拷贝开发机 semantic/models/e5s-v1/。Release: https://github.com/robotlover-1/Answermesh/releases/tag/models-e5s-v1"
  fi
fi

if [ -z "${DEEPSEEK_API_KEY:-}" ] || [ "${DEEPSEEK_API_KEY}" = "CHANGE_ME_sk-xxx" ] \
   || [ "${DEEPSEEK_API_KEY#sk-placeholder}" != "${DEEPSEEK_API_KEY}" ]; then
  echo "⚠ 未设置有效的 DEEPSEEK_API_KEY：openai-api-proxy 将使用占位 key，聊天会一直转圈（proxy 直连返回 401）。"
  echo "  推荐：写进 $FRPC_ENV 的这一行，之后 ./start.sh 裸跑即可 ——"
  echo "        DEEPSEEK_API_KEY=sk-xxx"
  echo "  或在启动时带上（仅当上面那个文件不存在时才生效，文件优先）："
  echo "        DEEPSEEK_API_KEY=sk-xxx ./start.sh"
  echo "  改完必须 ./stop.sh 再起：proxy 只在进程启动时读这个变量，重启才会拿到新值。"
  echo "  离线/无 key 调试：把 openai-api-proxy/dev.config.yaml 的 base_url 改回 http://localhost:8083/v1（mock）"
fi

echo "== 启动服务 =="

# ---- frpc 公网隧道（可选；配置/二进制缺失则跳过，不影响本地 9 个服务）----
# FRP_DOMAIN：从渲染好的 frpc.yaml 里回读出来的域名，仅供结尾摘要显示。
# **不要拿 PUBLIC_DOMAIN 存这个**——用户可以只靠环境变量传 PUBLIC_DOMAIN，在这里置空
# 会把它的输入清掉，于是配置被判成"未配置"（曾因此踩坑，见 commit 说明）。
FRP_DOMAIN=""
ensure_frpc() {
  local i rc out
  # 配置来源二选一，优先 deploy/app/.env（完整部署用它，还带向量库/DeepSeek 等其它键）：
  #   A) 文件：deploy/app/.env
  #   B) 环境变量：PUBLIC_DOMAIN / FRP_SERVER_ADDR / FRP_AUTH_TOKEN（+ 可选 FRP_BIND_PORT）
  # 只想开公网入口时 B 更省事，不必先 cp 一份 .env。
  local cfg_src=""
  if [ -f "$FRPC_ENV" ]; then
    cfg_src="file"
  elif [ -n "${PUBLIC_DOMAIN:-}" ] && [ -n "${FRP_SERVER_ADDR:-}" ] && [ -n "${FRP_AUTH_TOKEN:-}" ]; then
    cfg_src="env"
  else
    echo "  [$FRPC_NAME] ⚠ 跳过：公网入口未配置（不影响本地 http://localhost:7080）"
    echo "    配法二选一："
    echo "      A) 环境变量：PUBLIC_DOMAIN=<域名> FRP_SERVER_ADDR=<云端IP> FRP_AUTH_TOKEN=<与云端一致> ./start.sh"
    echo "      B) 配置文件：cp deploy/app/.env.example deploy/app/.env 后填写"
    echo "    前置：bin/frpc 客户端 + 一台跑着 frps 的云主机（deploy/edge），见 README「公网访问」"
    return 2
  fi
  if [ ! -x "$FRPC_BIN" ] && [ "${ECHO_FETCH_FRPC:-}" = "1" ]; then
    echo "  [$FRPC_NAME] 未找到 $FRPC_BIN，自动下载（ECHO_FETCH_FRPC=1）..."
    bash "$BASE/deploy/scripts/fetch_frpc.sh" \
      || echo "  [$FRPC_NAME] ✘ frpc 下载失败，可稍后单独跑 deploy/scripts/fetch_frpc.sh"
  fi
  if [ ! -x "$FRPC_BIN" ]; then
    echo "  [$FRPC_NAME] ⚠ 跳过：未找到 $FRPC_BIN"
    echo "    获取：bash deploy/scripts/fetch_frpc.sh    # 或 ECHO_FETCH_FRPC=1 ./start.sh 自动下"
    echo "    frp 0.62.1 客户端，须与云端 frps 同版本，见 README「公网访问」"
    return 2
  fi

  # 端口有默认值：模板里是 ${FRP_BIND_PORT}，envsubst 不支持 :- 兜底，只能在这里补齐。
  FRP_BIND_PORT="${FRP_BIND_PORT:-39000}"; export FRP_BIND_PORT

  # 复用 deploy/scripts/lib.sh 的 .env 解析与受限渲染（与 deploy-app.sh 同一套，含残留变量守卫）
  echo "  [$FRPC_NAME] 渲染配置 ... (来源: $([ "$cfg_src" = file ] && echo "$FRPC_ENV" || echo '环境变量'))"
  if ! out="$(bash -c '
        set -euo pipefail
        source "$1"
        if [ "$6" = file ]; then require_env "$2" "$3"; fi
        guard_required_vars PUBLIC_DOMAIN FRP_SERVER_ADDR FRP_AUTH_TOKEN
        render_restricted "$4" "$5" "\${FRP_SERVER_ADDR} \${FRP_BIND_PORT} \${FRP_AUTH_TOKEN} \${PUBLIC_DOMAIN}"
      ' _ "$BASE/deploy/scripts/lib.sh" "$FRPC_ENV" "$FRPC_ENV.example" \
        "$FRPC_TEMPLATE" "$FRPC_CONFIG" "$cfg_src" 2>&1)"; then
    echo "  [$FRPC_NAME] ✘ 配置渲染失败:"
    printf '%s\n' "$out" | sed 's/^/    /'
    return 1
  fi
  FRP_DOMAIN="$(grep -A1 'customDomains:' "$FRPC_CONFIG" | grep -oE '"[^"]+"' | tr -d '"' | head -1)"

  if frpc_registered; then
    echo "  [$FRPC_NAME] ✔ already running (隧道已建立 → $FRP_DOMAIN)"
    return 0
  fi

  echo "  [$FRPC_NAME] starting ..."
  # exec 不可省：没有它，`( cd X && cmd & )` 的子 shell 会自己留一层进程，
  # $! 记到的是这个转瞬即逝的中间 shell 而非 frpc（stop.sh 便会 TERM 到死 pid）。
  # exec 让 frpc 顶替子 shell，$! 即 frpc 本尊，且中间层消失不会拖住 start.sh。
  _frpc_pf="$(pidfile "$FRPC_NAME")"
  ( cd "$BASE" && exec nohup "$FRPC_BIN" -c "$FRPC_CONFIG" >>"$(logfile "$FRPC_NAME")" 2>&1 </dev/null & echo $! > "$_frpc_pf" )
  for i in $(seq 1 20); do
    if frpc_registered; then
      echo "  [$FRPC_NAME] ✔ 已登录 frps，公网入口就绪 → https://$FRP_DOMAIN"
      return 0
    fi
    sleep 0.5
  done
  echo "  [$FRPC_NAME] ✘ 未连上 frps（核对 token / FRP_SERVER_ADDR / 两端版本）, 日志尾部:"
  tail -n 5 "$(logfile "$FRPC_NAME")" 2>/dev/null | sed 's/^/    /'
  if frpc_token_mismatch; then
    echo "    ↑ 云端 frps 明确回报 token 不匹配：本机的 FRP_AUTH_TOKEN 必须与云端"
    echo "      deploy/edge/.env 里的那个**完全一致**（两端填同一个值，不能各生成一次）。"
  fi
  return 1
}

start_one() {
  local name="$1" port="$2" cwd="$3" cmd="$4" i
  if port_listening "$port"; then
    echo "  [$name] ✔ already running ($port)"
    return 0
  fi
  echo "  [$name] starting ..."
  ( cd "$cwd"; nohup $cmd >"$(logfile "$name")" 2>&1 & echo $! > "$(pidfile "$name")" )
  for i in $(seq 1 30); do
    port_listening "$port" && { echo "  [$name] ✔ $port listening"; return 0; }
    sleep 0.5
  done
  echo "  [$name] ✘ 启动失败, 日志尾部:"
  tail -n 5 "$(logfile "$name")" 2>/dev/null | sed 's/^/    /'
  return 1
}

total=0; ok=0; failed=(); frpc_state=""
for entry in "${SERVICES[@]}"; do
  IFS='|' read -r name port cwd cmd <<< "$entry"
  total=$((total+1))
  if start_one "$name" "$port" "$cwd" "$cmd"; then ok=$((ok+1)); else failed+=("$name"); fi
done

# frpc 隧道在本地服务就绪后再起：它反代 7080，后端没起来隧道通了也没意义。
# 不计入本地服务的 total/ok：它没监听端口，混在一起会让 "N/N 服务就绪" 失去意义。
if ensure_frpc; then frpc_state="up"; else
  [ $? -eq 2 ] && frpc_state="skip" || { frpc_state="fail"; failed+=("$FRPC_NAME"); }
fi

echo
if [ ${#failed[@]} -eq 0 ]; then
  echo "✔ $ok/$total 服务就绪 → http://localhost:7080"
  case "$frpc_state" in
    up)   echo "  公网入口 → https://$FRP_DOMAIN（frpc 隧道已建立）" ;;
    skip) echo "  公网入口未启用（本机访问不受影响）→ 需要公网访问见 README「公网访问」" ;;
  esac
else
  echo "✘ ${#failed[@]} 个失败: ${failed[*]}; 日志在 runtime/logs/, 用 ./stop.sh 清理后重试"
  exit 1
fi
