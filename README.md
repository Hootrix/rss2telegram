# rss2telegram

Go 语言编写的 RSS 订阅推送机器人，可以将 RSS 源的更新实时推送到 Telegram 频道/群组。

## 功能

- 🚀 支持多个 RSS 源订阅
  - RSS源支持多个 Telegram 频道/群组推送
- 🎨 自定义消息模板（支持 Markdown 格式），自动转换RSS源中的HTML为MD格式
- 🛡️ 自动过滤 30 天以前的旧文章
  - 自动清理过期的文章记录（30 天）
- ⚡️ 可靠的推送机制
  -  消息发送失败自动重试（普通错误最多 3 次尝试，429 最多 5 次重试）
  -  程序意外终止后的状态恢复，防止重复推送
- 🎉 配置文件修改后自动应用，无需重启服务
- 📸 可选的 per-feed 全文快照
  - 抓取文章原文并用 readability 提取正文，快照发布到 [Telegraph](https://telegra.ph)，推送消息中引用快照链接
  - 快照失败不阻塞推送，自动降级为原文链接


## 配置文件
默认在 `config/config.yaml` 中配置你的 RSS 源和 Telegram 频道：
配置文件也可以使用`-config`参数指定

[config/config.example](config/config.yaml.example#L1)


## 🐳运行

建议`docker`方式运行

`config.yaml`配置在当前目录下的`rss2telegram-config`文件夹中，运行命令:
```
# 启动
$ docker run -d --restart=always --name rss2telegram  -v $(pwd)/rss2telegram-config:/app/config  ghcr.io/hootrix/rss2telegram 

# 停止
$ docker stop rss2telegram

# 查看运行日志
$ docker logs -f rss2telegram

```

> **⚠️ 挂载注意事项**：请始终挂载**目录**（如上例 `-v $(pwd)/rss2telegram-config:/app/config`），不要挂载单个配置文件（如 `-v config.yaml:/app/config/config.yaml`）。
> 单文件挂载会把 inode 钉死，宿主机上编辑器保存（临时文件 + rename 覆盖）后容器内看到的仍是旧文件，配置热更新会完全失效。
> 程序启动时会自检挂载方式：若检测到单文件挂载，日志会输出 `[挂载方式告警]` 提示改用目录挂载。
>
> **平台差异**：Linux 原生 Docker（推荐的生产环境）下，宿主机编辑器保存后容器内自动热更新。
> Docker Desktop（Mac/Windows 本地开发）的虚拟文件系统不跨 VM 边界传播文件事件，宿主机保存后需 `docker restart` 才会应用新配置（文件数据本身会同步，仅事件通知不到达容器）。
>
> **已知限制**：配置热更新通过监听配置文件**所在目录**实现（应对编辑器原子保存替换 inode）。若 `config.yaml` 本身是指向其他目录文件的**符号链接**，写入目标文件的事件不会落在被监听的目录内，热更新会静默失效——请直接放置实体文件。

也可以手动下载`releases`页面提供的最新版本二进制程序


## 配置说明

### Telegram 配置
- `token`: Telegram Bot Token，从 [@BotFather](https://t.me/BotFather) 获取
- 确保你的 Bot 已被添加到目标频道，并具有发送消息的权限

### RSS 源配置
- `name`: RSS 源名称（用于日志记录）
- `url`: RSS 源地址
- `channels`: 要推送到的 Telegram 频道列表（格式：@channel_name）
- `snapshot`: 可选快照后端，缺省不启用；目前仅支持 `telegraph`，详见 [全文快照](#全文快照)
- `snapshot_source`: 可选快照正文来源，缺省 `page`（抓原文）；`feed` 表示正文直接取 RSS item（全文输出型源用），详见 [全文快照](#全文快照)；需同时启用 `snapshot`
- `media`: 可选媒体推送模式，缺省不启用；目前仅支持 `photo`（图片为主体，template 渲染结果为图片 caption），详见 [图片推送](#图片推送)
- `template`: 消息模板，支持 Markdown 格式，可用变量：
  - `{title}`: 标题
  - `{link}`: 链接
  - `{content}`: 内容（如果有）
  - `{description}`: 描述（如果有）
  - `{pubDate}`: 发布时间（如果有）
  - `{telegraph}`: 快照链接（启用 `snapshot: telegraph` 时可用；降级时为原文链接，无 link 为空串）

### 模板语法
- **变量语法**:
  - `{ field }` - 带空格的花括号语法，例如：`{ title }` 支持正则中使用`{}`
  - `{field}` - 紧凑的花括号语法，例如：`{title}`
  - 两种语法效果相同，可以根据个人偏好选择使用
- **支持的操作符**:
  允许对RSS数据进行提取/过滤/替换操作
  - `extract`: 使用正则表达式提取内容，例如：`{description|extract:([\S]+?市)}`
  - `extract-all`: 使用正则表达式提取所有匹配项，例如：`{title|extract-all:(\d+折)}`
  - `prefix`: 有生成内容时添加前缀，例如：`{title|extract-all:(\d+折)|prefix:#}`, `{title|extract:(\d+折)|prefix:#}`
  - `replace`: 使用正则表达式替换内容，例如：`{ description|extract:价格：(\d+)元|replace:\d{4}:**** }`
  - `default`: 设置默认值，当内容为空时使用，例如：`{description|extract:类型：(.*?)，|default:未知}`
- **操作符语法**:
  - 使用 `|` 分隔多个操作符
  - 操作符参数使用 `:` 分隔
  - 支持链式操作，例如：`{field|op1:param1|op2:param2}`
- **Markdown 转义注意** (issue #4):
  - `{title}` 的内容会自动转义 Telegram Markdown 特殊字符（`_` `` ` `` `*` `[` 前加 `\`），防止标题含裸 `*`/`[` 时 Telegram 拒收 400；模板里手写的 Markdown 语法不受影响
  - 因此 `extract`/`extract-all`/`replace` 的正则匹配的是**转义后**的 title 文本：原文 `3*4` 已变为 `3\*4`，存量配置中针对这些字符的匹配规则需相应调整（如用 `[*]` 匹配星号）
  - 不要把 `{title}` 放进 URL 位置（如 `[x]({link}?q={title})`）：转义产生的 `\` 会进入 URL 导致坏链
  - Markdown 解析仍失败时（如 description 含裸特殊字符），当前消息任务自动降级为纯文本；后续重试保留降级状态，不再重复发送已知无法解析的 Markdown 请求

### 文章处理机制
- **文章过期时间**: 默认 30 天，超过此时间的文章将被自动过滤
- **去重策略**: 
  - 优先使用文章的 GUID
  - 如无 GUID，使用文章链接
  - 如无链接，使用标题和发布时间的组合
  - 最后使用文章内容的哈希值
- **发布时间处理**:
  - 优先按发布时间排序（从旧到新）
  - 支持处理无发布时间的文章
  - 无发布时间的文章将按照 RSS 源中的顺序推送

### 推送控制
- **重试机制**: 普通错误最多 3 次尝试（含首次），指数退避；429 按服务端 `retry_after` 加 1 秒缓冲，最多 5 次重试，不占普通错误配额；缺失或异常的 `retry_after` 按普通错误处理
- **发送间隔**: 同一频道标识在进程内共享成功发送后至少 3 秒的间隔及 429 冷却，不再额外对整个 feed 执行发送后的等待；忙频道快速延后，其他频道可继续处理
- **累计预算**: 每个 feed 及每条消息默认使用 2 分钟累计 deadline，覆盖 RSS 请求、频道冷却、退避和 Telegram 请求；单个 feed 积压或源站挂起只耗尽自己的预算，不影响其他 feed；发送阶段超时视为正常延后，未发送的消息由后续检查补推；退出取消会中断等待及网络请求
- **长冷却处理**: 剩余冷却超过 10 秒时快速延后该频道，不占用 feed 名额长期等待；保留完整服务端 `retry_after` 加 1 秒缓冲的截止时间，重试耗尽也不清除冷却，未成功消息不标记已处理
- **频道标识**: `@name`、裸名和大小写别名共享限流状态；同一频道请统一使用用户名或数字 ID，不要混用两种标识，直接发送时无法关联它们指向的同一聊天
- **状态持久化**: 使用布隆过滤器保存已发送文章的状态，防止重复推送；限流状态仅在当前进程内保留，重启后如仍受限会根据新的 429 响应恢复冷却
- **退出边界**: Telegram 已确认成功的消息仍会尝试持久化 seen，避免取消后重复发送；文件系统写入与同步不支持 context 取消，因此 deadline 不是对磁盘故障或不可中断操作的硬实时保证

### 全文快照

摘要型 RSS 源可以在推送时把文章全文快照到 Telegraph 并在消息中引用，其他 feed 不受影响：

```yaml
- name: special-feed
  url: https://example.com/rss
  channels: ["@my_channel"]
  snapshot: telegraph      # 可选，缺省不启用；其他值启动报错
  snapshot_source: feed    # 可选，缺省 page（抓原文）；全文输出型 RSS 源配 feed
  template: |
    📰 *{title}*
    🔗 [快照]({telegraph})
    🌐 [原文]({link})
```

- **工作方式**: 抓取原文（15s 超时，响应上限 5MB，非 HTML 直接降级）→ charset 转码 → readability 提取正文（少于 200 字视为失败）→ 转换为 Telegraph 节点并按官方限制预截断（标题 256 / 作者 128 / 正文 64KB，超长自动追加"查看原文"提示）→ 发布（10s 超时）。快照耗时计入 feed 预算，积压时每轮推送量相应下降
- **正文来源 `snapshot_source`**（可选）:
  - `page`（缺省）: 抓原文 + readability 提取，即上述流程
  - `feed`: 跳过原文抓取，正文直接取 RSS item（`content:encoded` 优先，为空取 `description`），不经过 readability。适用于**全文输出型 RSS 源**——不受反爬、JS 渲染、编码问题影响，最坏耗时只剩发布 10s。正文质量校验与 page 同阈值（纯文本不足 200 字且无有效图片，或乱码占比超 1%，即降级为原文链接；**纯图帖豁免字数下限**，见下）；内容缺失**不会回退**抓原文（要抓原文就配 `page`）
- **开关只控制"是否创建快照"，消息内容完全由模板决定**: 只备份不想在消息中体现，模板不写 `{telegraph}` 即可；多频道差异化可用同一 URL 配置多条 feed（name/channels/template 各自不同）
- **失败降级**: 快照任一环节失败时 `{telegraph}` 渲染为原文链接（无 link 时为空串，注意 `[快照]()` 坏链风险，可用 `{telegraph|default:...}` 自行兜底），消息照发，日志记录原因；feed 预算耗尽或程序退出时延后到下一轮，不降级
- **缓存**: 同一 feed 的同一文章只建一次快照（进程内缓存 24 小时，上限 1024 条）；进程重启后最坏会为同一文章重复建一页（可接受，不做持久化去重）
- **凭证**: 自动创建匿名 Telegraph 会话并持久化在数据目录；token 丢失/损坏自动重建，旧页面不受影响

`snapshot_source: feed` 对源的要求（自建 RSS 生成服务参照）：
- 正文 HTML 写入 `<content:encoded>`（推荐）或 `<description>`，CDATA 包裹；正文片段即可（无需完整 html/body）
- 图片 `src` 必须是绝对 `http(s)` URL；相对地址、`data:`、`//cdn` 协议相对地址的图片一律丢弃（正文照建）
- 正文纯文本不少于 200 字，**或含至少 1 张有效图片**（纯图帖豁免字数下限，同样建页；两者皆无才降级）；`<link>` 保留文章原始链接

已知限制：
- **JS 渲染站点不适配**：原文页面是纯前端渲染（无服务端正文）时提取会失败，按上述规则降级为原文链接；此类站点的正路是把全文写进 RSS（如上 `snapshot_source: feed`），站点适配放在 RSS 生成层而非本 bot
- **图片防盗链**：Referer 防盗链的图片在 Telegraph 上无法加载（Telegraph 图片上传接口已关闭，无法转存）；懒加载图片会尝试取 `data-src` 真实地址
- **非 http(s) 图片丢弃**：所有快照（page/feed 来源均生效）中 `data:`/相对地址的图片会被直接丢弃而非生成坏图；page 来源经 readability 处理后相对地址已绝对化，实际仅影响 `data:` 内嵌图。**纯图帖**（正文不足 200 字）靠有效图片豁免字数下限建页——若图片全部被此规则丢弃（如全是 `data:` 内嵌图）则退回降级
- **防盗链图对纯图帖影响最大**：Referer 防盗链的图在 Telegraph 页面可能不显示（无法转存），纯图帖 worst case 页面只剩标题与短文本
- **首刷限流**: `first_push` 批量建页若触发 Telegraph 限流，被限流的文章降级照发并标记已推送，该批文章将没有快照
- **作者链接**: 消息发送成功后自动把 Telegraph 页面作者链接回填为该条消息的链接（`t.me/<频道>/<消息ID>`，仅公开频道）；回填失败或纯数字频道 ID 时保留频道主页/不填。回填为尽力而为：进程重启前所建的旧快照页（缓存丢失）不回填

## 图片推送

配置 `media: photo` 的 feed 以图片消息（`sendPhoto`）推送：图片作为消息主体，`template` 渲染结果作为图片下方 caption：

- 图片来源优先级：RSS item 的 `enclosure`（`image/*`，SVG/GIF 除外）> 正文（`content`）中的图片 > 摘要（`description`）中的图片，按序收集最多 3 个候选（正文图收满 3 个后不再取摘要图）、逐个尝试下载
- 图片由**本程序所在主机下载后上传**（需主机能访问图床），支持 jpeg/png/webp，单张 ≤ 10MB
- 图片超过 Telegram 尺寸限制（宽高和 >10000 或宽高比 >20）时：可切成 ≤10 张可读切片（片高 ≤1280 基本不压缩）则以相册推送；否则自动改发原文件（不压缩、无尺寸限制）；webp 无法本地解码、不参与切片，走上传失败文本降级
  - `photo_slices`：相册切片上限（1-10，默认 10；1 = 单图预览顶部切片）
  - `photo_overlimit`：超限长图行为，`document`（默认，整图发文件附件）或 `crop`（顶部切片相册预览并注明截断）
- 短边 < 50px 的小图（追踪像素/图标）、GIF/SVG、下载失败时该条自动回退普通文本推送
- caption 上限 1024 字符（Telegram 限制，纯文本消息为 4096），超长自动截断；建议把 `{link}` 放在 `{description}` 之前，避免链接被截掉
- 可与 `snapshot: telegraph` 叠加（先建快照，再发图片消息，caption 中 `{telegraph}` 可用）

## 许可证

MIT License
