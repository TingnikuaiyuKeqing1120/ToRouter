# Qoder 兼容包

来源：[`magpie-community/plugins`](https://github.com/magpie-community/plugins/tree/d34f78b69a303a620307f1202a77e171a13ecf8f/packages/qoder)，固定提交 `d34f78b69a303a620307f1202a77e171a13ecf8f`，`@magpie-community/opencode-qoder-auth` 0.2.19。

原始 `index.mjs` SHA256：`30235a28094366f2e7aa1cce904d63749f338bcc68de0f6308b79685baf38b98`。保留社区 MIT `LICENSE` 及包内 `LICENSE-CLIProxyAPI`；后者记录其 Qoder 协议来源。此处是直接采用和修改代码，不仅是研究参考。

2026-10-20 to-router 修改：

- 独立设备授权路径；宿主只暴露方法 0，不调用签到、原工具运行时、PAT 或 VPC 分支。不是 Node 沙箱。
- `contract.mjs` 明确请求边界，未知控制、严格工具、强制工具选择、不完整工具历史等明确拒绝；历史明文思考随消息保留，上游接受性待真实账号验证。
- 不添加固定代理角色提示，不把强制工具选择降为一句提示词；默认保留厂商默认上下文窗口，不根据估算自动升档。
- 流必须读到厂商终态；断流不补成功。交错工具按原索引汇集并保留原 ID；缺失用量不补零。自有测试使用合成上游事件，不证明真实服务兼容。
- 宿主启动前核对打包文件摘要；不执行 npm 安装脚本、不跟随 latest。取消／启停及加密写回由 to-router 管理。

生产包仅包含 `index.mjs`、`contract.mjs` 和两份许可文本。测试及来源说明不进入可执行插件目录。后续升级须重新核对 hooks、请求语义及这些反例。
