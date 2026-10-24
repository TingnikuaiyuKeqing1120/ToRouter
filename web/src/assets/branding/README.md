# to-router 品牌与图标库规范 (Official Icon Suite)

本目录为 `to-router` 官方定型的吉祥物与视觉资产库。涵盖 Claude Code 吉祥物 **Clawd** 与 DeepSeek 蓝鲸吉祥物的共生设计。

## 1. 资产分工与定型方案 (Asset Roles & Mapping)

| 资产文件 | 设计方案 | 视觉要素 | 承担职责与场景 |
| :--- | :--- | :--- | :--- |
| `tray-icon.svg`<br/>`internal/tray/assets/tray.ico` | **Mascot-2: 鲸蟹双生太极徽标** | 纯白陶瓷 Squircle 底板 + 蓝鲸与暖赤陶蟹阴阳循环 + 中心发光 Nexus Hub 路由节点 | **Windows 托盘驻留图标 (System Tray)**<br/>多尺寸 ICO (16×16 至 256×256) |
| `app-icon.svg`<br/>`web/public/favicon.svg`<br/>`web/public/favicon.ico` | **方案 1-C: 骑鲸冲浪** | 黑曜石底板 + 1,431 像素官方蓝鲸 + 416 像素纯白腹/眼填充 + 戴耳机 Clawd + 8-bit 海浪 | **应用与桌面图标 / 浏览器 Favicon**<br/>桌面启动入口与网页标头 |
| `ui-logo-dark.svg` | **方案 1-A: 斜点分线 (暗色版)** | 黑曜石渐变底板 + 1,431 像素蓝鲸 + 彩色斜点数据流 + 右下坐镇 Clawd | **Web 控制台品牌徽标 (Dark Theme)**<br/>配合暗色模式或登录卡片 |
| `ui-logo-light.svg` | **方案 1-B: 斜点分线 (亮色版)** | 纯白陶瓷底板 + 1,431 像素蓝鲸 + 彩色斜点数据流 + 右下坐镇 Clawd (软赤陶色) | **Web 控制台品牌徽标 (Light Theme)**<br/>配合浅色侧边栏导航 |

---

## 2. 核心参数与几何规格

- **像素蓝鲸模型**：100% 提取自 MakeBead 官方点阵图（62×62 网格，共计 1,431 个蓝色像素），腹部 404 像素与眼部 12 像素均经实心纯白打底，杜绝暗底透光。
- **Clawd 像素小蟹**：标准双钳四足像素体，头戴天空蓝与深海蓝配色的高保真音频耳机。
- **斜点路由数据流**：五枚彩色圆角数据包自右上向左下穿梭，依次为：
  - Cyan (`#38BDF8`) → Emerald (`#10B981`) → Amber (`#F59E0B`) → Indigo (`#818CF8`) → Cyan (`#38BDF8`)
- **React 组件入口**：`web/src/BrandLogo.tsx`，支持 `size`、`theme` (`'auto' | 'dark' | 'light'`) 与 `showSquircle` 自适应属性。
