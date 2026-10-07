#!/usr/bin/env bash
# Telegraph 快照链路本地集成冒烟（issue #12）
#
# 全离线自包含：RSS 源、原文 HTML、Telegram API、Telegraph API 全部指向本地假服务器，
# 验证 main.go 装配 → processFeed 快照编排 → extractor → telegraph client → 消息渲染全链路。
#
# 用法: ./scripts/integration_telegraph.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${PORT:-18931}"
WORK="$(mktemp -d /tmp/rss2telegram-smoke.XXXXXX)"
# trap 内 wait 收集被 TERM 杀死的后台进程返回 128+N，set -e 会立即中止 shell
# （后续 rm -rf 不再执行、退出码泄漏成 143），必须 || true 兜底
trap 'kill ${BOT_PID:-} ${FAKE_PID:-} 2>/dev/null; wait ${BOT_PID:-} ${FAKE_PID:-} 2>/dev/null || true; rm -rf "$WORK"' EXIT

echo "==> 工作目录: $WORK"
echo "==> 构建主程序与假服务器"
go build -o "$WORK/rss2telegram" "$ROOT/cmd"
go build -o "$WORK/faketelegraph" "$ROOT/scripts/faketelegraph"

echo "==> 启动 faketelegraph (port $PORT)"
"$WORK/faketelegraph" -addr "127.0.0.1:$PORT" \
  -root "$ROOT/testdata/telegraph_smoke" \
  -pages "$WORK/pages.jsonl" -edits "$WORK/edits.jsonl" -sent "$WORK/sent.jsonl" &
FAKE_PID=$!

mkdir -p "$WORK/config"
cat > "$WORK/config/config.yaml" <<EOF
telegram:
  bot_token: "1:smoke"
  check_interval: 2

feeds:
  - name: smoke-feed
    url: http://127.0.0.1:$PORT/s/rss.xml
    channels: ["@smoke_ch"]
    snapshot: telegraph
    first_push: true
    template: |
      📰 *{title}*
      🔗 [快照]({telegraph})
      🌐 [原文]({link})
EOF

echo "==> 启动主程序（假 Telegram/Telegraph 注入）"
TELEGRAM_API_URL="http://127.0.0.1:$PORT" \
TELEGRAPH_API_URL="http://127.0.0.1:$PORT" \
  "$WORK/rss2telegram" -config "$WORK/config/config.yaml" &
BOT_PID=$!

# 轮询等待首条推送落地（check_interval=2s，快照全链路本地 <1s）；
# edits.jsonl 也纳入等待：回填发生在 sendMessage 之后，晚于 sent.jsonl 落地
for _ in $(seq 1 15); do
  [ -s "$WORK/sent.jsonl" ] && [ -s "$WORK/pages.jsonl" ] && [ -s "$WORK/edits.jsonl" ] && break
  sleep 1
done
sleep 1 # 等日志落盘
kill -INT $BOT_PID 2>/dev/null || true
wait $BOT_PID 2>/dev/null || true

echo "==> 校验推送与建页记录"
FAIL=0

# 推送消息包含假 Telegraph 快照链接
if grep -q 'telegra.ph/smoke-1' "$WORK/sent.jsonl"; then
  echo "PASS: 消息包含快照链接"
else
  echo "FAIL: 消息未包含快照链接"; cat "$WORK/sent.jsonl"; FAIL=1
fi

# 建页请求含标题/作者（feed 名）/频道主页链接
PAGE=$(head -1 "$WORK/pages.jsonl")
for expect in '快照冒烟文章' 'smoke-feed' 'https://t.me/smoke_ch'; do
  if grep -qF "$expect" <<<"$PAGE"; then
    echo "PASS: 建页含 $expect"
  else
    echo "FAIL: 建页缺少 $expect"; FAIL=1
  fi
done

# token 已持久化（0600）
TOKEN_FILE="$WORK/config/rss2telegram-data/telegraph_token.json"
if [ -f "$TOKEN_FILE" ]; then
  echo "PASS: telegraph token 已持久化"
else
  echo "FAIL: telegraph token 未持久化"; FAIL=1
fi

# 只建一页（同 feed 同文章缓存命中，仅一个频道）
if [ "$(wc -l < "$WORK/pages.jsonl" | tr -d ' ')" = "1" ]; then
  echo "PASS: 只建一页"
else
  echo "FAIL: 建页数异常"; FAIL=1
fi

# author_url 回填（issue #12）：editPage 请求定位到所建页面，author_url 为消息链接
# 假 Telegram 固定返回 message_id=1 → 链接为 t.me/smoke_ch/1；
# path 与真实 API 一致不带前导斜杠（缓存 Path 已 TrimPrefix 页面 URL 前缀）
EDIT=$(head -1 "$WORK/edits.jsonl")
for expect in '"path":"smoke-1"' '"author_url":"https://t.me/smoke_ch/1"' '快照冒烟文章'; do
  if grep -qF "$expect" <<<"$EDIT"; then
    echo "PASS: 回填请求含 $expect"
  else
    echo "FAIL: 回填请求缺少 $expect"; cat "$WORK/edits.jsonl" 2>/dev/null; FAIL=1
  fi
done

# 回填仅重提交不新建页（editPage 不进 pages.jsonl 已由"只建一页"隐含，这里显式断言恰好一次编辑）
if [ "$(wc -l < "$WORK/edits.jsonl" | tr -d ' ')" = "1" ]; then
  echo "PASS: 恰好一次回填"
else
  echo "FAIL: 回填次数异常"; cat "$WORK/edits.jsonl"; FAIL=1
fi

if [ "$FAIL" = 0 ]; then
  echo "==> 冒烟通过 ✅"
  exit 0 # 显式退出码：EXIT trap 中 wait 已终止进程的状态会泄漏成 128+N
else
  echo "==> 冒烟失败 ❌"; exit 1
fi
