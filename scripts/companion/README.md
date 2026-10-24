# 小小搭档 · 六状态独立演示

直接打开 [index.html](index.html) 即可离线观看。页面没有外部字体、脚本、图片或网关请求；底部的来源链接仅在点击后打开上游仓库。

- **打个盹**：蓝鲸微呼吸、偶尔摆尾，Clawd 沿用原版趴睡与浮动 Z。
- **认真工作**：蓝鲸使用 deepwork 参考轮廓，轻点头、摆尾和喷水；Clawd 保留原版敲键盘、看屏幕与代码行变化。
- **乘风破浪**：Clawd 站在鲸背上抛接彩球，鲸与骑手一起起伏；水波与小水花表达前进。
- **修修就好**：蓝鲸短暂惊醒，然后安静等待；Clawd 戴帽挥锤维修。读数提示重试，不继续累加 Token。
- **查个明白**：蓝鲸收住动作，头顶保留低频红色警示；Clawd 拿放大镜排查。点击只回应气泡，告警和排查动作保持。
- **收工啦**：Clawd 跳两下，蓝鲸点头喷小水花，约 2.8 秒后自动回到待机。暂停时庆祝和回退计时一起冻结。

底部切换六个场景，也可按 `1` 至 `6`。前三个场景点击角色播放短暂庆祝，结束后回到当前动作；重试、故障与完成状态只给气泡回应。右上角可以暂停、放大或切换深浅背景；自动切换时普通场景停留 12–16 秒，完成状态为 2.8 秒。手动切换会停止轮播、取消当前点击反馈，并重新开始目标场景动作。暂停会同时冻结角色动作、蓝鲸轮廓变化、模拟计数和场景计时。系统偏好减少动态效果时默认暂停。

本 HTML 仍是独立视觉原型，读数均为模拟值。正式程序的 `#/hud` 已复用这套动画并接入真实网关请求；使用方法与计量边界见[根 README](../../README.md#桌面伴侣)。此原型不访问路由、数据库或托盘。

## 修改与生成

```powershell
node scripts/companion/build.mjs
```

- `index.template.html`：场景、界面、交互与动作编排的源文件。
- `build.mjs`：将原版 Clawd 动画隔离命名，清理蓝鲸 SVG 的外部背景、字标及不相连的图块，生成单文件 HTML，同时导出正式页面的 `web/src/companion/mascot.generated.ts`、`stage.css` 与 `web/public/companion/` 来源/许可。无需安装依赖；前端 `predev` / `prebuild` 自动运行。
- `web/src/companion/CompanionStage.tsx`：正式 React 舞台，独立处理 SVG 实例 ID、暂停、后台、减少动态效果和点击反馈。父组件决定场景，真实请求状态不会读原型的模拟数据或计时器。
- `assets/deepsleep.svg`、`assets/deepwork.svg`、`assets/deepwake.svg`：用户提供的原始参考，保留不改。惊醒场景从 deepwake 提取眼部曲线，沿用已经认可的身体轮廓。
- `assets/whale-sleep.svg`、`assets/whale-work.svg`、`assets/whale-wake.svg`：从原参考生成的轮廓；每次构建更新，勿直接编辑。
- `assets/clawd-*.svg`：上游原始动画，保留不改。构建时对内联副本的 CSS、keyframes 和 ID 加前缀，避免各状态互相覆盖；界面另对工作/杂耍节奏作小幅调整。

蓝鲸的尾巴通过同一条闭合轮廓连续变化，避免拆成两块后在连接处露缝。除活动场景外的动作暂停；浏览器切到后台时暂停更新，回来后继续，不补跳后台时间。

## 素材来源

- Clawd 动画来自 [rullerzhou-afk/clawd-on-desk](https://github.com/rullerzhou-afk/clawd-on-desk/tree/main/assets/svg)，对应所选 GIF 的原始可编辑 SVG。下载日期与上游 Git blob 记录在 [assets/SOURCES.json](assets/SOURCES.json)。
- 上游根 `LICENSE` 是 GNU Affero General Public License v3，原文随附于 [assets/CLAWD-LICENSE.txt](assets/CLAWD-LICENSE.txt)。本目录保留原动画源文件、修改代码及来源；正式构建也嵌入来源清单和许可全文。
- 蓝鲸由用户提供；用户说明其为“网络流传二创图，原型为 DeepSeek 官方图标”。二创作者、具体原始发布链接及再分发授权仍未知，不能据此声称已获授权或官方背书。

本批范围与实际验收记录见 [T070](../../docs/archive/tasks/2026-10-06-companion-animation.md)。

统一声明见 [THIRD_PARTY_NOTICES](../../THIRD_PARTY_NOTICES.md)。`python scripts/check-third-party.py` 从已安装的锁定依赖和固定来源文本生成 `/third-party/notices.html`，`--check` 离线检查漂移。Clawd 的原始 SVG、许可、场景模板和生成脚本随声明页提供；这份素材源快照不能代替组合程序所需的全部对应源代码。本项目尚未选择整体许可证，也尚未确定公开对应源代码的提供方式。
