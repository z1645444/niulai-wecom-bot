# 企业微信智能机器人语音消息能力（官方文档核验）

核验日期：2026-08-21。以下结论以企业微信开发者中心当前页面为准；页面显示“智能机器人长连接”最后更新于 2026/05/25。

## 结论

支持发送语音。对于本项目使用的 API 模式 WebSocket 长连接，`aibot_send_msg`（主动推送）和 `aibot_respond_msg`（收到 `aibot_msg_callback` 后回复）均可使用 `msgtype: "voice"`。

语音消息格式为：

```json
{
  "msgtype": "voice",
  "voice": {
    "media_id": "MEDIA_ID"
  }
}
```

`media_id` 需要先通过长连接文档中的 `aibot_upload_media_init`、`aibot_upload_media_chunk`、`aibot_upload_media_finish` 上传临时语音素材后取得。长连接文档的“主动推送消息 → 消息类型格式说明”明确列出 `voice`，并给出上述请求结构；“回复普通消息”也明确列出语音消息。

注：同一页面“主动推送消息 → 请求格式”的 `body.msgtype` 参数表目前只展开显示 `template_card` 和 `markdown`，与紧随其后的消息类型格式表（含 `file`、`image`、`voice`、`video`）不完全一致。实现语音时应以该页明确的 `voice.media_id` 示例及官方 SDK 的媒体发送接口为交叉核对依据；必要时在企业微信接口调试工具中实测。

## 接口限制

- 主动推送的 `chatid` 支持单聊和群聊；但企业微信要求用户先在该会话中给机器人发过消息，机器人才能对该会话主动推送。
- 回复或主动推送合计按会话限流：30 条/分钟、1000 条/小时。
- 收到消息回调后，24 小时内可以向该会话回复消息。
- 入站回调中的 `msgtype: "voice"` 表示“用户发送的语音（转为文本）”，官方表格注明仅支持单聊；这是入站能力限制，不影响出站语音消息格式。

## 与旧版“群机器人”Webhook 的区别

不要把“智能机器人 API 长连接”和旧版群机器人 Webhook 混为一谈。企业微信当前的“消息推送（原‘群机器人’）”文档（最后更新 2025/08/07）也明确写明自定义消息推送支持八种类型，其中包含 `voice`，格式同样是 `voice.media_id`。因此，基于当前官方文档，旧版群机器人 Webhook 也支持发送语音；不能再依据旧资料断言 Webhook 不支持语音。

## 官方来源

1. [智能机器人长连接（企业微信开发者中心）](https://developer.work.weixin.qq.com/document/path/101463) — 页面更新 2026/05/25；“支持的消息类型”“回复普通消息”“主动推送消息”“消息类型格式说明 → 语音消息”及临时素材上传章节。
2. [消息推送配置说明（原“群机器人”）](https://developer.work.weixin.qq.com/document/path/91770) — 页面更新 2025/08/07；“当前自定义消息推送支持……”列表及“语音类型”章节。
3. [WeComTeam/aibot-node-sdk（企业微信智能机器人 SDK）](https://github.com/WecomTeam/aibot-node-sdk) — 官方 SDK README 将 `sendMediaMessage` 的媒体类型列为 `file`/`image`/`voice`/`video`，并说明该方法通过 `aibot_send_msg` 主动推送。

## 对本项目的影响

当前 `internal/wecom/client.go` 仅封装了 `markdown` 出站消息。若要发送语音，还需要增加临时素材上传流程，并让 `aibot_send_msg` 的 body 支持 `voice: { media_id }`；本研究未修改代码。
