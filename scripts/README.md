# scripts/ — 本地测试辅助资产

本目录**不随仓库提交**（untracked），仅本地使用。为 issue #3/#5 修复配套搭建的回归测试体系。

## 目录内容

| 文件 | 用途 |
|---|---|
| `faketelegram/main.go` | 假 Telegram Bot API 服务器：响应 `getMe`/`getChat`/`sendMessage` 并把发送的消息记录到 jsonl；同时静态伺服 `/rss/` 下的 XML，充当可控 RSS 源 |
| `stalebloom/main.go` | 把目录下 `.bloom` 文件头 8 字节时间戳改写为 N 天前（模拟布隆过滤器老化），用于复现 issue #5 的重启重推场景 |
| `integration_test.sh` | 宿主机直跑二进制的端到端集成回归（5 场景） |
| `integration_test_docker.sh` | Docker 容器方式的端到端集成回归（5 场景，构建临时镜像 `rss2telegram-it:ci`） |
| `../testdata/sample.xml` | 线上 RSS 源不可达时的离线兜底样例（2 条文章） |

## 集成测试场景对照

**宿主机版 `integration_test.sh`**：
1. 冷启动 `first_push:false` 不推存量
2. RSS 出现新文章 → 推送增量
3. vim 式原子保存（tmp+rename）触发热更新，新 feed 生效并首推 ← **issue #3 核心**
4. 正常重启进程 → 去重状态持久化，无重推
5. bloom 时间戳老化 35 天 + 重启 → 仍无重推 ← **issue #5 端到端**

**容器版 `integration_test_docker.sh`**：
- D1 容器冷启动不推存量
- D2 宿主机 vim 保存经 bind mount 同步进容器（`docker cp` 验证）
- D3 热更新生效（见下方平台差异）
- D4 `docker restart` 后去重状态持久化，无重推
- D5 单文件挂载 → 启动输出 `[挂载方式告警]`；目录挂载不误报（反向对照）

## 用法

```bash
# 宿主机版（依赖 go、curl；curl 拉线上真实源失败自动退回内置样例）
scripts/integration_test.sh

# 容器版（额外依赖 docker；假 API 通过 host.docker.internal 供容器访问）
scripts/integration_test_docker.sh
```

两者均通过 `TELEGRAM_API_URL` 环境变量把发消息指向本地假 API，不触碰真实 Telegram。

## 平台差异（容器版 D3）

- **Linux 原生 Docker**（线上环境）：inotify 事件穿透 bind mount，宿主机保存即断言容器内自动热更新（`Config Reloaded` ≥ 2 次）
- **Docker Desktop（mac/win）**：文件事件不跨 VM 边界传播（数据同步、事件不到达，已实验证实），断言自动降级为 `docker restart` 兜底路径

## 失败排查

任一断言失败时脚本退出并在最后一行打印保留的临时工作目录（`/tmp/rss2telegram-*-it.*`），内含 `app.log`、`fake.log`、`sent.jsonl` 等现场。
