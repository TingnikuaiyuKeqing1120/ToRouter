# WorkBuddy 兼容包

直接采用并修改 [`magpie-community/plugins` 的 WorkBuddy 包](https://github.com/magpie-community/plugins/tree/d34f78b69a303a620307f1202a77e171a13ecf8f/packages/workbuddy)，固定提交 `d34f78b69a303a620307f1202a77e171a13ecf8f`，`@magpie-community/opencode-workbuddy-auth` 版本 `0.1.25`。

原始 `index.mjs` SHA256：`b4dc427ae1e357c117a7fc29d2b3042f6f39e584f4900ca2eb3f862f28fed2ae`。
`LICENSE` 原字节复制自同提交仓库根目录的 MIT 文本，SHA256：`bebc3d1ec74f197b010ed6f6b6743dbae73fe4865d71bd0670b0a4e203056679`。

2026-10-20 to-router 改动：

- 两区仅保留独立浏览器授权、模型查询、令牌刷新和 Chat Completions。移除桌面授权及文件读取、用量、签到、原客户端配置和 `chat.headers` hook；宿主只装配 `config`、`provider`、`auth`。
- 请求原文发出；移除 `withSystem`、`unflag`、`fitted`、`normalizeToolChoice`、`replyAfterTools` 及模型拒绝后重发。发网络前拒绝已知无法保持的请求控制、消息角色、严格工具约束和工具形状，不改提示词或 schema。上游只接受 `stream:true`；非流式汇总由宿主承担。
- 授权 URL 只允许固定地区的产品域：CN 为 `copilot.tencent.com`、`www.codebuddy.cn`、`www.workbuddy.cn`；AI 为 `www.workbuddy.ai`、`www.codebuddy.ai`。这些产品域亦见固定 Magpie WorkBuddy 实现；其它 HTTPS 域明确拒绝。
- 所有请求的 origin 固定为当前地区 API。`domain` 仅验证为纯 DNS 主机名后用于 `X-Domain`，企业动态域不参与网络选址。企业归属查证失败或歧义时拒绝授权；`uid` 和 `enterpriseId` 交给宿主组成稳定身份。账号默认名称用产品名，不取手机号、昵称。
- 刷新不吞错误，不保留可绕过保存的内存令牌缓存；内联刷新必须等 `client.auth.set` 可靠保存确认才发出推理或模型请求。显式 `auth.refresh` 由宿主保存返回状态；不因网络错误重发推理，不补查 meter。
- 独立授权生成并持久保存随机 UUID `sessionID`，状态加载时校验格式；会话及轮次请求头由它和原始历史派生，刷新完整保留。派生不使用令牌或进程随机盐，跨 worker 重启和工具回填保持稳定，不同授权种子互不碰撞。
- 控制 API 失败只输出自有文案、HTTP 状态和数字代码，不复制上游正文、状态文本或网络异常内容。首次授权及新令牌尚未保存时，不依赖宿主已知凭证做事后脱敏。
- 实时模型查询失败或空列表时返回失败，无静态清单回退。保留来源的模型配置解析与企业请求头。
- SSE 普通数据保留原字段、工具 ID、独立索引、拒绝及原始用量；不补零。必须有真实 `finish_reason` 才承认终态，孤立 `[DONE]`、断流及非法事件返回错误，不在 EOF 合成成功。读至传输 EOF 时，重复 `[DONE]` 或其后数据也明确返回错误。HTTP/SSE 错误调用宿主脱敏器，响应不转发非必要诊断头。
- `Retry-After` 在 128 字符以内保留原值，秒数和 HTTP 日期的有效性统一由现有 Go 网关解析，避免丢失日期形式的冷却信息；429／503 的两种形式均有合成断言。

生产包仅需 `index.mjs`、`contract.mjs`、`LICENSE`；来源说明及 `contract.test.mjs` 不进入可执行插件目录。测试使用合成 fetch，未访问真实账户或厂商网络；Node.js `v24.13.0` 下 `node --test internal/pluginhost/packages/workbuddy/contract.test.mjs` 的 17 项通过，不证明真实厂商接受度或跨平台运行。
