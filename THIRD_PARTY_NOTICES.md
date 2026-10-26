# 第三方声明

to-router 使用第三方运行依赖、参考实现与直接采用的素材；三类关系分别记录，不能由这里的分类推断全仓代码均未复制或许可已全部满足。项目自有代码采用 MIT 许可证（见根 [LICENSE](LICENSE)）；各第三方部分适用其相应条款，Clawd 动画另受素材许可与 ToRouter 非商业专项许可约束，不纳入 MIT。本文件不代替各许可及授权原文的法律效力。

完整的许可、版权与 NOTICE 原文、锁定版本和 SHA256 在 [随程序提供的声明页](web/public/third-party/notices.html)及[机器可读清单](web/public/third-party/manifest.json)。运行程序后从设置中的第三方声明入口打开 `/third-party/notices.html`；三个入口共用嵌入的 `web/dist`，无需联网即可读取许可原文。外部来源链接仅在用户点击后联网。

## 运行依赖与构建范围

T120 的 OpenCode Zen Free 原生接入参考并改编 Magpie community `opencode-zen-free-auth` 0.1.11 的目录、请求身份与流聚合逻辑，采用MIT许可；固定版本、保留原文及差异见[来源说明](internal/zenfree/SOURCE.md)。没有采用其CLI运行、本地登录读取或补入客户端未声明工具的路径。

- Go：按 Windows amd64、`production` 构建标签枚举 `cmd/torouter`、`cmd/torouter-terminal`、`cmd/torouter-gui` 的实际导入图；逐项标出使用它的入口，保留所属模块及其内附子组件的许可。Wails、go-webview2 的内附许可单独保留；Go 运行时、标准库及源码树内第三方许可也一并保留。
- 前端：从 `web/package-lock.json` 取生产依赖图，核对已安装包的名称及精确版本，并保留各包随附的许可和 NOTICE。此图是保守覆盖，可能包含仅供类型检查或被打包器移除的代码，不宣称每个包或包内文件均进入成品。
- 开发／测试：清单保留完整 Go 构建列表和 npm 锁定清单，分别标记发布入口／生产图；不把开发工具全都描述为发布依赖，也不宣称已收集所有开发依赖的许可原文。
- 当前构建不捆绑 Microsoft WebView2 系统运行时。将来若单独捆绑安装器／运行时，须先核实并补齐相应再分发条款。现有 Go 包和其 loader 的许可已保留。

模块内附的测试素材等许可也可能被保守收集。自动化按随包法律文本文件名收集，不能证明任意源文件头、未声明依赖或外部素材都已获得授权；新增复制代码、字体、图像、二进制、压缩包时仍需核对具体来源。

## 参考实现与设计研究

[固定来源登记](third_party/references.json)保留原始文本的下载地址与 SHA256；[上游原文](third_party/upstream/)不改写作者姓名、合法邮箱或版权信息。

| 参考项目 | 固定研究版本 | 已记录范围 |
|---|---|---|
| Codex-Manager | `4e63e68d9e4e89aa3ed38e1ca76dffb735326404`、`f14bd735dca9203d7430958480b82ba7713641a9` | OAuth、额度解析、重置卡等；见协议资料及 OAuth 设计 |
| CLIProxyAPI | `8ef43e4df3b216a42493105d31c2873b69191473` | 凭证轮换、额度信号与冷却研究 |
| CC Switch | `372b1698968ced988ce046d705732cb799000533` | Copilot OAuth 来源参考、配置与产品边界研究；更早浅克隆版本未记录 |
| New API | `1a4166d8e8ba9802d2ca56fe8ecf0ed5404e80d5` | 网关与重试机制研究；保留 LICENSE 和 NOTICE |
| LiteLLM | `9a5e828310efac7cc6646d4725d38cec1ea61f21` | 部署、回退与重试研究；根许可中的 enterprise 范围区分保留 |
| OpenAI Codex | `afb436df8b70bb5bc57b86d9a3e829968988cd21` | 额度窗口、请求头与重置卡协议来源；保留 LICENSE 和 NOTICE |

这些记录以已有[研究归档](docs/archive/research/2026-10-04-personal-gateway-research.md)、[协议资料](docs/protocol-refs/README.md)及[OAuth 设计](docs/design/oauth-information.md)为依据。本轮未进行全仓复制代码溯源审计，不将历史研究中的“本轮没有复制”扩展成对整个项目的证明。

## 直接采用与改编的素材

**供应商市场与Bun宿主（T126）**：`internal/pluginhost/market.mjs`的原包加载、OpenCode／Magpie auth／provider／config、auth.loader及SDK auth.set语义参考并改编自Magpie固定版本`b437e4adb4dba82446e0d625b040a8e2d95004b4`的`internal/plugin/host.js`；目录／下载及运行时准备参考同版`market.go`、`installcache.go`、`bun.go`等。保留[MIT原文](third_party/upstream/magpie-b437e4ad/LICENSE)，含合法版权署名。本地改编只承接供应商接口，并接入to-router的私有stdio、Store状态确认、权限、流背压及取消，不包含Magpie Go内置、签到、Agent／工具／页面或网关中间件功能。

**社区目录数据（T126）**：`internal/pluginmarket/registry.json`来自`magpie-community/plugins@a1689b1b6feb37f9de4c213fee276bc9be4676cb`的`registry.json`，保留其中22条供应商记录作为冷启动回退，排除其它类型；不是未经修改的完整目录。保留[社区MIT原文](third_party/upstream/magpie-plugins-a1689b1b/LICENSE)，固定来源与SHA256登记在[references.json](third_party/references.json)。运行时更新的公开目录、独立第三方条目及其图标仍各归原作者，不因“Magpie社区”角标表示模型厂商背书。

市场所需Bun是可选运行时：优先复用本机可用版本，否则从Bun官方release按固定SHA256下载1.4.2；Bun二进制没有打包进to-router EXE。本程序参考的下载选择与校验代码适用上述Magpie MIT许可，Bun本体及其内附组件适用各自上游条款，不把它们统称为本项目MIT代码。用户安装的供应商原包及依赖保留下载包内的许可，未将所有远端可安装包宣称为EXE内附依赖或已获全量许可审查。

**CC Switch与Magpie图标（T122）**：分别从官方仓库固定`farion1231/cc-switch@f92ff0b03964188f1cd011d9302ab8869d7a8219`的`src-tauri/icons/128x128.png`及`yetone/magpie@8febe3a50c1b5a51b53d7c2e6a3223cc66e6ccfe`的`internal/gui/icon-256.png`复制原始PNG。各自MIT许可原文在`third_party/upstream/ccswitch-f92ff0b0/`与`third_party/upstream/magpie-8febe3a5/`；来源与SHA256见图标catalog，原素材及许可随离线声明提供。仅用于识别工具，不表示背书。

**Qoder 兼容包（T120 C）**：直接采用并修改 Magpie community `opencode-qoder-auth` `0.2.19 / d34f78b69a303a620307f1202a77e171a13ecf8f` 的 JavaScript 实现，按需安装到本机兼容层。保留社区 MIT 许可与包内 `LICENSE-CLIProxyAPI`，随离线声明提供。请求边界、历史思考、交错工具、真实终态和用量处理的改动见[来源记录](internal/pluginhost/packages/qoder/SOURCE.md)。不开放签到、原工具运行时调用或 OAuth 自动读取；兼容代码以当前用户权限运行，不是 OS 沙箱。Node.js 运行时由用户另行安装，本程序没有打包 Node.js 二进制。

**原生订阅接入实现（T120）**：Claude OAuth、固定请求身份、CCH规范化／签名向量、uTLS ClientHello及有序HTTP/1.1写入改编自CLIProxyAPI固定版本`e7d4c7316fca3fe0e852866f4e611cb4f2fa0764`（MIT）；精确文件与修改边界见[来源记录](internal/claude/SOURCE.md)，保留[原许可](third_party/upstream/CLIProxyAPI-e7d4c731/LICENSE)。Antigravity授权、项目初始化和请求封装参考同一CPA版本及Magpie `ef655acb40b2b2b5484e8eab41c79ff8b41bb6d3`（MIT），见[来源记录](internal/antigravity/SOURCE.md)及[Magpie原许可](third_party/upstream/magpie-ef655acb/LICENSE)。新增运行依赖uTLS、xxHash、Brotli、klauspost/compress及附带许可按实际构建图收集；这些来源和本地测试不代表上游厂商认可或真实账号接入已获验证。

**新增厂商／工具图标**为[Lobe Icons](https://github.com/lobehub/lobe-icons)固定版本`8a1b2821bb768c484fe8da20c5e1838b6bf551a9`的34个静态SVG，采用其[MIT许可原文](third_party/upstream/lobe-icons-8a1b2821/LICENSE)。[素材清单](web/src/assets/provider-icons/catalog.json)记录逐项来源路径和SHA256，SVG原文保持不变；彩色图直接呈现，单色图由界面按主题文字颜色遮罩显示。无需新增npm依赖或运行时CDN访问。许可与素材原文随声明页离线提供，生成器核对漂移；品牌名称与图标用于识别对应厂商／工具，不表示其为to-router背书或授权使用其服务。

**供应商市场图标补齐（T127）**：Cloudflare Workers AI、Cline、Kiro、Trae、Command Code、Nous Research、SenseNova的7个SVG复用上述Lobe固定版本；Factory、Zed、MiMo Code、WorkBuddy的4个SVG及ZCode的PNG逐字复制自`yetone/magpie@b437e4adb4dba82446e0d625b040a8e2d95004b4`的`internal/gui/assets/icons/`，保留[Magpie MIT原文](third_party/upstream/magpie-b437e4ad/LICENSE)。逐项固定路径、版本及SHA256见图标catalog；素材与许可随离线声明提供，仅用于品牌识别，不表示厂商背书或其服务能力已验证。Open Design、AutoClaw、CodeArts未采用缺少可靠对应来源的图标，继续显示通用图标。

**Devin原生接入（T120 B2）**：独立PKCE、模型目录与私有Connect RPC参考Magpie社区Devin插件`0.1.20 / d34f78b69a303a620307f1202a77e171a13ecf8f`及上述CPA版本；保留[社区仓库MIT许可](third_party/upstream/magpie-plugins-d34f78b6/LICENSE)。Go实现映射为无服务端存储的Responses，不采用源工具登录跟随、CLI执行、提示词改写、虚构工具结果或Token估算；字段依据与改编边界见[Devin来源记录](internal/devin/SOURCE.md)。

**Clawd 动画**：动画素材来自 [clawd-on-desk](https://github.com/rullerzhou-afk/clawd-on-desk)，动画作者：鹿鹿（[@rullerzhou-afk](https://github.com/rullerzhou-afk)）。[经作者于 2026-10-25 同意](https://github.com/rullerzhou-afk/clawd-on-desk/issues/1259#issuecomment-6398719885)，用于 ToRouter 的非商业使用、改编及随项目分发，**不包含任何商业用途的授权**。同意仅涉及作者创作并有权授权的动画部分。

上游源码的 AGPL-3.0 不覆盖动画与美术素材。这里保留独立的 [assets/LICENSE 原文](scripts/companion/assets/CLAWD-LICENSE.txt)（All Rights Reserved）与[ToRouter 专项许可说明](scripts/companion/assets/CLAWD-PERMISSION.md)，并随程序提供。许可原文固定于上游提交 `464aa948641afa64cb097c3f92612fbfd2a5634e`；6 个原始 SVG 的 Git blob、许可来源／SHA256 与作者回复链接记录在 [SOURCES.json](scripts/companion/assets/SOURCES.json)，生成器核对原始 blob 及许可摘要。

这部分实际复制 SVG 动画代码与图形，经 `scripts/companion/build.mjs` 对标识符、CSS 与 keyframes 加前缀，再组合场景和调整节奏，自 2026-10-06 起修改。最终内联进 `web/src/companion/mascot.generated.ts` 并打包；不能仅称为设计借鉴。可编辑的原始 SVG、场景模板及修改生成脚本在仓库 `scripts/companion/`，也随声明页提供[素材源文件快照](web/public/third-party/sources/companion/)。源文件的提供不改变素材的非商业许可范围。

**蓝鲸**由用户提供 `deepsleep.svg`、`deepwork.svg`、`deepwake.svg`。用户说明其为“网络流传二创图，原型为 DeepSeek 官方图标”。当前改编包括裁剪轮廓、简化扫描线转角、提取眼部与尾部连续形变。二创作者、具体原始发布链接及再分发授权仍未知，不声称获得上游许可或官方背书。素材保持现状；公开分发前必须解决授权依据或经用户另行决定处理方式。

## 素材许可范围

Clawd 动画不纳入 ToRouter 自有代码的 MIT 许可，不能因源码公开、署名或提供素材源文件而推导商业使用权。保留素材许可原文和作者回复中的非商业范围；如需超出此次许可范围，须另行取得相应权利人的许可。

Clawd 角色本身的权利归 Anthropic；作者对其有权授权的动画所作的同意，不代表 Anthropic 的官方授权或背书，也不涉及蓝鲸等第三方素材的授权。蓝鲸的来源及授权未知状态不因本次 Clawd 许可确认而改变。

## 维护与校验

在已有 Go 模块缓存、已运行 `npm ci` 的 `web/node_modules` 上离线执行：

```powershell
python scripts/check-third-party.py
python scripts/check-third-party.py --check
```

生成命令刷新 `web/public/third-party/`；`--check` 不写文件，对输入、法律文本、素材源文件或生成产物漂移返回失败。脚本禁止自动联网拉取 Go 模块；缺缓存、缺生产包、版本不符、缺许可、来源摘要不符均失败，不静默漏项。参考项目文本是已核实的固定来源快照；更新研究版本时须更新原文及登记，再重新生成。

先生成声明，再运行前端构建，使 Vite 将声明复制到 `web/dist`，最后编译三端。复用既有 `web/dist` 时须校验其中的声明与当前生成结果一致，不能用最新源文件声明替代旧二进制所带内容。SHA256 用于追踪原文与变化，不是授权证明。
