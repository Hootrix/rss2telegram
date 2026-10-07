#!/usr/bin/env bash
# rss2telegram Docker 容器端到端集成回归测试
# 与 integration_test.sh 互补：验证容器方式启动的完整链路
#   - bind mount 目录挂载（线上同款方式）下宿主机保存的配置同步进容器
#   - 容器内进程的热更新（issue#3 在容器场景的验证，仅 Linux 原生 Docker）
#   - TELEGRAM_API_URL 环境变量注入在容器内生效
#   - docker restart 后去重状态持久化（挂载卷）无重推
#   - 单文件挂载启动时输出挂载方式告警（目录挂载不误报）
#
# 平台差异（重要）：
#   Linux 原生 Docker（线上环境）：inotify 事件穿透 bind mount，宿主机保存即可热更新
#   Docker Desktop（mac/win 本地开发）：VirtioFS/gRPC-FSEvents 不跨 VM 边界传播事件，
#     宿主机保存后数据会同步但事件不会到达容器 → 热更新断言自动降级为
#     “restart 兜底路径”（与修复前用户手动 restart 的操作等价）
#   （已用独立 fsnotify 实验验证：mac Docker Desktop 下容器内零事件、数据同步正常）
#
# 依赖: go、docker、curl（拉线上真实 RSS 样例，失败退回内置样例）
# 用法: scripts/integration_test_docker.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d /tmp/rss2telegram-docker-it.XXXXXX)"
PORT="${IT_PORT:-18925}"
HOST_BASE="http://host.docker.internal:$PORT"   # 容器内访问宿主机 faketelegram
# Darwin/Windows 视为 Docker Desktop（事件不穿透）；Linux 为原生
IS_DESKTOP="$(uname -s)"
PASS=0; FAIL=0

cleanup() {
  docker rm -f rss2telegram-it rss2telegram-it-single >/dev/null 2>&1
  [ -n "${FAKE_PID:-}" ] && { kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null; }
  rm -rf "$WORK"
}
trap cleanup EXIT

assert() {
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then
    echo "  ✓ $desc"; PASS=$((PASS+1))
  else
    echo "  ✗ $desc  ← FAILED"; FAIL=$((FAIL+1))
  fi
}
sent_count() { [ -f "$WORK/sent.jsonl" ] && wc -l < "$WORK/sent.jsonl" | tr -d ' ' || echo 0; }
sent_eq() { [ "$(sent_count)" = "$1" ]; }
# Reloaded 计数：启动初始加载也会打一行，热更新断言必须用计数(≥2)而非存在性，避免假阳性
reload_count() { docker logs rss2telegram-it 2>&1 | grep -c "Config Reloaded"; }
reload_ge2() { [ "$(reload_count)" -ge 2 ]; }
docker_log_has() { docker logs rss2telegram-it 2>&1 | grep -q "$1"; }
docker_log_has_single() { docker logs rss2telegram-it-single 2>&1 | grep -q "$1"; }
# 目录挂载（正确用法）下不应出现挂载方式告警（单文件挂载检测的误报防护）
log_lacks_warning() { ! docker_log_has "挂载方式告警"; }
# 从容器拷出配置文件验证 bind mount 数据同步（distroless 无 shell，不能 exec cat）
container_config_has() { docker cp rss2telegram-it:/app/config/config.yaml "$WORK/cfg.copy.yaml" >/dev/null 2>&1 && grep -q "$1" "$WORK/cfg.copy.yaml"; }
wait_for() {
  local deadline=$(( $(date +%s) + $1 )); shift
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  return 1
}

echo "==> 工作目录: $WORK"
echo "==> 运行环境: $IS_DESKTOP $([ "$IS_DESKTOP" = "Linux" ] && echo '(原生 Docker，含事件热更新断言)' || echo '(Docker Desktop，热更新断言降级为 restart 兜底路径)')"

echo "==> 构建镜像（首次较慢，利用层缓存）"
(cd "$ROOT" && docker build -t rss2telegram-it:ci .) || exit 1

echo "==> 准备 RSS 样例与配置"
mkdir -p "$WORK/rss" "$WORK/cfgdir"
if curl -sf --max-time 15 "https://rss.hhtjim.com/apple.xml" -o "$WORK/real.xml" && grep -q '<item>' "$WORK/real.xml"; then
  REAL_N=$(grep -c '<item>' "$WORK/real.xml" || true)
  echo "  线上真实源: apple.xml（${REAL_N} 条）"
else
  cp "$ROOT/testdata/sample.xml" "$WORK/real.xml"
  REAL_N=$(grep -c '<item>' "$WORK/real.xml" || true)
  echo "  线上源不可达，使用内置样例（${REAL_N} 条）"
fi
cp "$WORK/real.xml" "$WORK/rss/second.xml"

cat > "$WORK/rss/small.xml" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>it-small</title><link>http://x/</link><description>it</description>
<item><title>旧文A</title><link>http://x/a</link><guid>A</guid></item>
<item><title>旧文B</title><link>http://x/b</link><guid>B</guid></item>
</channel></rss>
EOF

# feed URL 与 TELEGRAM_API_URL 均指向宿主机 faketelegram（host.docker.internal）
cat > "$WORK/cfgdir/config.yaml" <<EOF
telegram:
  bot_token: "IT-FAKE-TOKEN"
  check_interval: 2
feeds:
  - name: "small"
    url: "$HOST_BASE/rss/small.xml"
    first_push: false
    channels: ["@it_test"]
EOF
# 配置 v2：新增 second feed（first_push: true）
cat > "$WORK/cfgdir/config.v2.yaml" <<EOF
telegram:
  bot_token: "IT-FAKE-TOKEN"
  check_interval: 2
feeds:
  - name: "small"
    url: "$HOST_BASE/rss/small.xml"
    first_push: false
    channels: ["@it_test"]
  - name: "second"
    url: "$HOST_BASE/rss/second.xml"
    first_push: true
    channels: ["@it_test"]
EOF

echo "==> 启动假 Telegram API ($PORT, 宿主机)"
(cd "$ROOT" && go build -o "$WORK/faketelegram" ./scripts/faketelegram) || exit 1
"$WORK/faketelegram" -addr "127.0.0.1:$PORT" -rss "$WORK/rss" -log "$WORK/sent.jsonl" > "$WORK/fake.log" 2>&1 &
FAKE_PID=$!
sleep 0.5

echo ""
echo "== 场景D1: 容器冷启动 first_push:false，不推存量"
docker rm -f rss2telegram-it >/dev/null 2>&1
docker run -d --name rss2telegram-it \
  -v "$WORK/cfgdir:/app/config" \
  -e TELEGRAM_API_URL="$HOST_BASE" \
  rss2telegram-it:ci >/dev/null
assert "容器启动无崩溃（日志有 Bot started）"   wait_for 15 docker_log_has "Bot started"
assert "first_push 跳过且零推送（sent=0）"      wait_for 15 docker_log_has "First run and first_push is false"
assert "存量未推送"                             wait_for 10 sent_eq 0

echo ""
echo "== 场景D2: 宿主机 vim 式保存(tmp+rename) → 配置进容器（bind mount 验证）"
cp "$WORK/cfgdir/config.v2.yaml" "$WORK/cfgdir/config.yaml.tmp~"
mv "$WORK/cfgdir/config.yaml.tmp~" "$WORK/cfgdir/config.yaml"
assert "新配置已同步进容器（docker cp 可见 second feed）" container_config_has "second"

echo ""
echo "== 场景D3: 配置热更新生效路径"
if [ "$IS_DESKTOP" = "Linux" ]; then
  assert "事件穿透：不重启出现第2次 Config Reloaded"    wait_for 15 reload_ge2
  assert "新 feed(线上源 ${REAL_N} 条)首推（sent=${REAL_N}）" wait_for 25 sent_eq "$REAL_N"
else
  echo "  （Docker Desktop 事件不穿透，跳过事件断言，验证 restart 兜底路径）"
  docker restart rss2telegram-it >/dev/null
  assert "restart 后启动加载新配置，second feed 首推（sent=${REAL_N}）" wait_for 25 sent_eq "$REAL_N"
fi

echo ""
echo "== 场景D4: docker restart → 挂载卷去重状态持久化，无重推"
docker restart rss2telegram-it >/dev/null
sleep 8
assert "重启后无任何重推（sent 仍为 ${REAL_N}）" sent_eq "$REAL_N"
# 反向对照：正确目录挂载方式下绝不能出现挂载告警（防误报）
assert "目录挂载（正确用法）无挂载方式告警"      log_lacks_warning

echo ""
echo "== 场景D5: 单文件挂载 → 启动即输出挂载方式告警（引导用户改目录挂载）"
docker rm -f rss2telegram-it-single >/dev/null 2>&1
docker run -d --name rss2telegram-it-single \
  -v "$WORK/cfgdir/config.yaml:/app/config/config.yaml" \
  -e TELEGRAM_API_URL="$HOST_BASE" \
  rss2telegram-it:ci >/dev/null
single_log_has() { docker logs rss2telegram-it-single 2>&1 | grep -q "挂载方式告警"; }
assert "单文件挂载容器输出挂载方式告警"           wait_for 15 single_log_has
assert "告警容器仍正常运行（Bot started）"        wait_for 15 docker_log_has_single "Bot started"

echo ""
echo "==> 结果: PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ] || { echo "==> 失败详情可查: ${WORK}（已保留）"; trap - EXIT; }
exit $([ "$FAIL" -eq 0 ] && echo 0 || echo 1)
