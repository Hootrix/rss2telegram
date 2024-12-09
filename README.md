# rss2telegram

Go 语言编写的 RSS 订阅推送机器人，可以将 RSS 源的更新实时推送到 Telegram 频道/群组。

## 功能

- 🚀 支持多个 RSS 源订阅
  - RSS源支持多个 Telegram 频道/群组推送
- 🎨 自定义消息模板（支持 Markdown 格式），自动转换RSS源中的HTML为MD格式
- 🛡️ 自动过滤 30 天以前的旧文章
  - 自动清理过期的文章记录（30 天）
- ⚡️ 可靠的推送机制
  -  消息发送失败自动重试（最多 3 次）
  -  程序意外终止后的状态恢复，防止重复推送
- 🎉 配置文件修改后自动应用，无需重启服务


## 配置文件
默认在 `config/config.yaml` 中配置你的 RSS 源和 Telegram 频道：
配置文件也可以使用`-config`参数指定

[config/config.example](config/config.yaml.example#L1)


## 🐳运行

建议`docker`方式运行

`config.yaml`配置在当前目录下的`rss2telegram-config`文件夹中，运行命令:
```
# 启动
$ docker run -d --name rss2telegram  -v $(pwd)/rss2telegram-config:/app/config  ghcr.io/hootrix/rss2telegram 

# 停止
$ docker stop rss2telegram

# 查看运行日志
$ docker logs -f rss2telegram

```

也可以手动下载`releases`页面提供的最新版本二进制程序


## 配置说明

### Telegram 配置
- `token`: Telegram Bot Token，从 [@BotFather](https://t.me/BotFather) 获取
- 确保你的 Bot 已被添加到目标频道，并具有发送消息的权限

### RSS 源配置
- `name`: RSS 源名称（用于日志记录）
- `url`: RSS 源地址
- `channels`: 要推送到的 Telegram 频道列表（格式：@channel_name）
- `template`: 消息模板，支持 Markdown 格式，可用变量：
  - `{title}`: 标题
  - `{link}`: 链接
  - `{content}`: 内容（如果有）
  - `{description}`: 描述（如果有）
  - `{pubDate}`: 发布时间（如果有）

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
- **重试机制**: 发送失败自动重试，指数避让
- **发送间隔**: 每条消息发送后等待 1 秒，避免触发 Telegram 限制
- **状态持久化**: 使用布隆过滤器保存已发送文章的状态，防止重复推送

## 许可证

MIT License

