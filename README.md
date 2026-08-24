# 牛来 - 企业微信虚拟员工

一个基于企业微信智能机器人和 WebSocket 长连接的虚拟员工“牛来”在工作时间内随机触发“发妈妈”事件（每个群聊每天至少保底触发一次），收到包含“牛来”的文本后停止、回复完成消息（默认 🐮，可配置图片），并进入 2 小时冷却

## 快速开始

### 1. 安装依赖

```bash
make deps
```

### 2. 配置环境变量

复制示例文件并填写实际值：

```bash
cp .env.example .env
```

编辑 `.env`：

```bash
WECOM_BOT_ID=your-bot-id
WECOM_BOT_SECRET=your-bot-secret
TARGET_CHAT_ID=your-target-chat-id-1,your-target-chat-id-2
```

`TARGET_CHAT_ID` 支持使用逗号分隔配置多个会话。运行期间自动发现的新群聊会增量加入目标列表，已存在的 ID 不会重复添加；修改启动配置后请重启进程。

### 3. 运行

```bash
make run
```

或编译后运行：

```bash
make build
./build/niulai
```

## 构建跨平台二进制

```bash
make build-all
```

产物位于 `build/` 目录：

- `niulai-linux-amd64`
- `niulai-darwin-amd64`
- `niulai-darwin-arm64`

## 业务规则

详见 [CONSTRAINTS.md](./CONSTRAINTS.md)

## 项目结构

```text
.
├── cmd/niulai/        # 程序入口
├── internal/
│   ├── bot/           # 牛来业务编排
│   ├── config/        # 配置解析
│   ├── scheduler/     # 工作时间判断与随机触发
│   ├── state/         # 状态机
│   └── wecom/         # 企业微信 WS 客户端与消息协议
├── CONSTRAINTS.md     # 业务约束文档
├── Makefile           # 构建脚本
└── .env.example       # 环境变量示例
```

## 注意事项

- `WECOM_BOT_ID` 和 `WECOM_BOT_SECRET` 请通过环境变量注入，不要提交到代码仓库
- 服务以单进程方式运行，重启后状态重置为 `IDLE`
- `TARGET_CHAT_ID` 支持逗号分隔的多个会话 ID；留空时从回调中自动发现并增量加入
