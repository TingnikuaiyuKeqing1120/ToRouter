# 常用厂商与工具图标

2026-10-20取得27个Lobe Icons静态SVG；保留既有ProviderIcons图标与ID，新增资源由其统一呈现。选择以常见厂商、本轮涉及的工具及本地模型工具为主，不将整套图库加入成品。

- 固定上游：[lobehub/lobe-icons@8a1b2821bb768c484fe8da20c5e1838b6bf551a9](https://github.com/lobehub/lobe-icons/tree/8a1b2821bb768c484fe8da20c5e1838b6bf551a9)。
- [catalog.json](catalog.json)维护ID、名称、自动匹配词、呈现方式、原始路径和SHA256。SVG按原文字节保存，不粘贴成无来源的路径片段。
- 原始许可：[MIT](../../../../third_party/upstream/lobe-icons-8a1b2821/LICENSE)，版权原文保留。`python scripts/check-third-party.py`将许可／来源／SVG同步到程序内离线声明，`--check`检查漂移。
- `color`使用原彩色资源；`mono`使用当前文字色遮罩，适应深浅主题；不添加远程图片请求或执行SVG内容。
- 当前未取得确切独立素材的工具不以近似品牌冒充；例如CodeBuddy与WorkBuddy、Z.ai与ZCode不是同一图标条目。Qoder素材不用于默认为此前待澄清的QCode标识。

图标只表示身份，不承诺已实现对应平台的登录、模型请求或额度功能。新增图标同时更新前端目录及Store允许的ID；上传自定义图片仍受现有PNG持久化规则约束。

2026-10-21新增CC Switch与Magpie原仓库PNG图标，分别固定`f92ff0b`与`8febe3a`；原路径、完整版本、文件及MIT许可SHA256记录在catalog的对应条目，生成器逐项提供独立离线声明。PNG保持原始字节，无运行时热链。WorkBuddy继续消费插件明示的现有CodeBuddy图标引用，不将其声明为已核实独立WorkBuddy品牌素材。
