# MiniMax Code 兼容包

来源：[`magpie-community/plugins`](https://github.com/magpie-community/plugins/tree/d34f78b69a303a620307f1202a77e171a13ecf8f/packages/minimax)，固定提交 `d34f78b69a303a620307f1202a77e171a13ecf8f`，`@magpie-community/opencode-minimax-auth` 0.1.2。

原始 `index.mjs` SHA256：`4f6eae6febf8ef45e301bd0f8a9562fd3f588e97e5a45caf541f1d0bd3f9e5cd`。根 MIT `LICENSE` 原字节复制，保留合法署名。此包直接采用并修改源码，不仅是研究参考；不安装 npm 依赖或执行安装脚本。

2026-10-20 to-router 修改：

- 仅 CN／Global 独立设备 OAuth 方法 0；不读写原工具登录、Agent 配置，不暴露签到或额度 hooks。宿主 hooks 只有 `config`、`provider`、`auth`。
- 原生 Anthropic Messages（`@ai-sdk/anthropic`），API 分别为 `https://agent.minimax.cn/mavis/api/v1/llm/v1` 和 `https://agent.minimax.io/mavis/api/v1/llm/v1`。推理固定 POST `/messages`，请求体原字节保留；不修改提示词、历史、强制工具、思考或新参数。基础校验只识别 Messages 必需结构，不把未知原生扩展白名单化。厂商是否接受这些原生字段仍待实号验证。
- 去除 `chat.headers`。新授权生成 UUID `sessionID` 并随完整 OAuth 状态持久化，以该账号固定 `X-Mavis-Session-Id` 跨请求／刷新／子进程重启保持稳定。没有额外会话字段、提示词或本机配置依赖；该标识不表示厂商提供会话历史存储。
- 完整状态必填 `type=oauth`、`access`、`refresh`、`expires`（毫秒）、`uid`、`realUserID`（二者一致）、`sessionID`。稳定用户 ID 必须来自签名身份查询；不以邮箱、昵称或未知 JWT 字段作为身份兜底，不保存邮箱／昵称。新授权私有结果的 `accountId` 为稳定 ID，界面名称由宿主处理。
- 刷新等待 `client.auth.set` 确认完整状态耐久保存，再使用新材料。失败停止，不缓存未保存令牌、不吞错、不回退旧令牌、不因 401 自动再发推理。`auth.refresh` 内部保存后返回 undefined，避免宿主再次合并保存；跨进程串行、版本 CAS、取消中保存与轮转未知恢复由 Go 宿主负责。
- 模型只读实时目录，失败／空目录直接失败；不匿名重试、不退静态快照。没有从模型目录推导厂商实号可用性的承诺。
- 正常 SSE 帧保持原内容和用量，缺失用量保持缺失；必须收到完整 `message_stop` 才正常结束，缺终态／半帧／非法 JSON／传输失败均不补成功。HTTP 与 SSE 错误仅保留安全自有诊断，不向下游透传任意厂商错误正文。非流式必须有实际 stop_reason。取消转交原响应 reader。

网络范围（按地区固定，宿主再次核对，禁止跨域重定向）：

| 目的 | Method | CN | Global |
|---|---|---|---|
| 设备码 | POST | `https://account.minimax.cn/oauth2/device/code` | `https://account.minimax.io/oauth2/device/code` |
| 设备轮询／令牌轮转 | POST | `https://account.minimax.cn/oauth2/token` | `https://account.minimax.io/oauth2/token` |
| 稳定身份 | GET | `https://agent.minimaxi.com/v1/api/user/info` | `https://agent.minimax.io/v1/api/user/info` |
| 实时目录 | GET | `https://agent.minimax.cn/mavis/api/v1/models` | `https://agent.minimax.io/mavis/api/v1/models` |
| 推理 | POST | `https://agent.minimax.cn/mavis/api/v1/llm/v1/messages` | `https://agent.minimax.io/mavis/api/v1/llm/v1/messages` |

`/oauth2/token` 的 `grant_type=refresh_token` 消耗轮转材料；设备轮询使用 device_code grant。身份请求保留固定源的签名和查询参数。浏览器授权 URL 来自设备响应，只允许同地区 account HTTPS origin、无 userinfo／fragment，路径不超过 2048 字符，不假定未核实的固定厂商授权页面路径。`authorize.user_code` 单独返回设备验证码，供宿主校验和显示；不依赖第三方说明文本向用户传达验证码。

生产包只包含 `index.mjs`、`contract.mjs`、`LICENSE`；来源说明与测试不进入执行目录。`contract.test.mjs` 使用合成网络、合成账号和流事件，覆盖原生历史／参数、独立授权、持久化顺序、401 单发、目录失败、断流、工具交错、未知用量及错误脱敏。这些本地证据不证明真实厂商登录、账号资格、计费、推理或任意客户端兼容；不是 Node 沙箱。
