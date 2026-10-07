#!/usr/bin/env bash
# rss2telegram 端到端集成回归测试（不依赖真实 Telegram，发消息走本地假 API）
#
# 覆盖场景：
#   1. 冷启动 first_push:false 不推存量
#   2. RSS 增量 → 新文章推送
#   3. vim 式原子保存(tmp+rename)触发配置热更新（issue#3 核心），新 feed 生效并首推
#   4. 正常重启进程 → 去重状态持久化，无重推
#   5. bloom 时间戳老化 35 天 + 重启 → 仍无重推（issue#5 端到端）
#
# 依赖: go、curl（拉线上真实 RSS 样例，失败自动退回内置样例）
# 用法: scripts/integration_test.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d /tmp/rss2telegram-it.XXXXXX)"
PORT="${IT_PORT:-18923}"
BASE="http://127.0.0.1:$PORT"
PASS=0; FAIL=0
APP_PID=""; FAKE_PID=""

cleanup() {
  [ -n "$APP_PID" ] && { kill "$APP_PID" 2>/dev/null; wait "$APP_PID" 2>/dev/null; }
  [ -n "$FAKE_PID" ] && { kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null; }
  rm -rf "$WORK"
}
trap cleanup EXIT

assert() { # assert <描述> <条件命令...>
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then
    echo "  ✓ $desc"; PASS=$((PASS+1))
  else
    echo "  ✗ $desc  ← FAILED"; FAIL=$((FAIL+1))
  fi
}

sent_count() { [ -f "$WORK/sent.jsonl" ] && wc -l < "$WORK/sent.jsonl" | tr -d ' ' || echo 0; }
sent_has() { grep -q "$1" "$WORK/sent.jsonl" 2>/dev/null; }
log_has() { grep -q "$1" "$WORK/app.log" 2>/dev/null; }
sent_eq() { [ "$(sent_count)" = "$1" ]; }

wait_for() { # wait_for <超时秒> <条件命令...>
  local deadline=$(( $(date +%s) + $1 )); shift
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  return 1
}

start_app() { # 启动被测进程（复用 $WORK/cfgdir/config.yaml 与既有状态）
  TELEGRAM_API_URL="$BASE" "$WORK/app" -config "$WORK/cfgdir/config.yaml" >> "$WORK/app.log" 2>&1 &
  APP_PID=$!
}
stop_app() {
  [ -n "$APP_PID" ] && kill "$APP_PID" 2>/dev/null
  wait "$APP_PID" 2>/dev/null
  APP_PID=""
}

echo "==> 工作目录: $WORK"

echo "==> 编译被测二进制与工具"
(cd "$ROOT" && go build -o "$WORK/app" ./cmd) || exit 1
(cd "$ROOT" && go build -o "$WORK/faketelegram" ./scripts/faketelegram) || exit 1
(cd "$ROOT" && go build -o "$WORK/stalebloom" ./scripts/stalebloom) || exit 1

echo "==> 准备 RSS 样例"
mkdir -p "$WORK/rss" "$WORK/cfgdir"
# 线上真实源（issue 数据源），失败退回内置样例
if curl -sf --max-time 15 "https://rss.hhtjim.com/apple.xml" -o "$WORK/real.xml" && grep -q '<item>' "$WORK/real.xml"; then
  REAL_N=$(grep -c '<item>' "$WORK/real.xml" || true)
  echo "  线上真实源: apple.xml（${REAL_N} 条）"
else
  cp "$ROOT/testdata/sample.xml" "$WORK/real.xml"
  REAL_N=$(grep -c '<item>' "$WORK/real.xml" || true)
  echo "  线上源不可达，使用内置样例（${REAL_N} 条）"
fi
cp "$WORK/real.xml" "$WORK/rss/second.xml"

# feed1: 手写小源，精确控制增量（v1=2 条，v2=3 条多一条新文章）
cat > "$WORK/rss/small.xml" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>it-small</title><link>http://x/</link><description>it</description>
<item><title>旧文A</title><link>http://x/a</link><guid>A</guid></item>
<item><title>旧文B</title><link>http://x/b</link><guid>B</guid></item>
</channel></rss>
EOF
cat > "$WORK/rss/small.v2.xml" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>it-small</title><link>http://x/</link><description>it</description>
<item><title>旧文A</title><link>http://x/a</link><guid>A</guid></item>
<item><title>旧文B</title><link>http://x/b</link><guid>B</guid></item>
<item><title>新文章C-增量测试</title><link>http://x/c</link><guid>C-NEW</guid></item>
</channel></rss>
EOF

cat > "$WORK/cfgdir/config.yaml" <<EOF
telegram:
  bot_token: "IT-FAKE-TOKEN"
  check_interval: 2
feeds:
  - name: "small"
    url: "$BASE/rss/small.xml"
    first_push: false
    channels: ["@it_test"]
EOF

echo "==> 启动假 Telegram API ($BASE)"
"$WORK/faketelegram" -addr "127.0.0.1:$PORT" -rss "$WORK/rss" -log "$WORK/sent.jsonl" > "$WORK/fake.log" 2>&1 &
FAKE_PID=$!
sleep 0.5

echo ""
echo "== 场景1: 冷启动 first_push:false，不推存量文章"
start_app
assert "日志出现 first_push 跳过标记"        wait_for 10 log_has "First run and first_push is false"
assert "存量 2 条均未推送（sent=0）"          wait_for 10 sent_eq 0

echo ""
echo "== 场景2: RSS 出现新文章，推送增量"
mv "$WORK/rss/small.v2.xml" "$WORK/rss/small.xml"
assert "新文章 C 被推送（sent=1）"            wait_for 10 sent_eq 1
assert "推送内容含新文章标题"                  sent_has "新文章C-增量测试"

echo ""
echo "== 场景3: vim 式原子保存（tmp+rename）触发热更新，新 feed 生效（issue#3 核心）"
cat > "$WORK/cfgdir/config.yaml.tmp~" <<EOF
telegram:
  bot_token: "IT-FAKE-TOKEN"
  check_interval: 2
feeds:
  - name: "small"
    url: "$BASE/rss/small.xml"
    first_push: false
    channels: ["@it_test"]
  - name: "second"
    url: "$BASE/rss/second.xml"
    first_push: true
    channels: ["@it_test"]
EOF
mv "$WORK/cfgdir/config.yaml.tmp~" "$WORK/cfgdir/config.yaml"
assert "进程不重启即出现 Config Reloaded"     wait_for 10 log_has "Config Reloaded"
assert "新 feed(线上源 ${REAL_N} 条)完成首推（sent=$((1+REAL_N))）" wait_for 20 sent_eq "$((1+REAL_N))"
assert "second feed 处理日志出现"              wait_for 10 log_has "name:second"

echo ""
echo "== 场景4: 正常重启进程，去重状态持久化，无重推"
stop_app
start_app
sleep 6
assert "重启后无任何重推（sent 仍为 $((1+REAL_N))）" sent_eq "$((1+REAL_N))"

echo ""
echo "== 场景5: bloom 时间戳老化 35 天 + 重启，仍无重推（issue#5 端到端）"
stop_app
"$WORK/stalebloom" -dir "$WORK/cfgdir/rss2telegram-data" -days 35 >> "$WORK/app.log" 2>&1
start_app
sleep 8
assert "35 天老化后重启仍无重推（sent 仍为 $((1+REAL_N))）" sent_eq "$((1+REAL_N))"

echo ""
stop_app
echo "==> 结果: PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ] || { echo "==> 失败详情可查: ${WORK}（已保留）"; trap - EXIT; }
exit $([ "$FAIL" -eq 0 ] && echo 0 || echo 1)
