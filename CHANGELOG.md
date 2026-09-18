# Changelog

## Unreleased

- 未发布的改动记录在这里。

## 0.1.4

- 拒绝 `${METIS_DIR_*}` 旧宿主目录占位符，避免旧路径契约被原样注入容器。
- 保留 MPK 和部署包中的 overlay 文件、嵌套目录及空目录条目，确保目录挂载在升级和重部署时一致。

## 0.1.2

- 要求常驻 Compose service 声明 `restart: unless-stopped`，并允许显式标记一次性 service。
- 增加 `MPK-COMPOSE-RESTART` 门禁规则。
- 收紧 overlay source 契约：只接受规范化的 `./overlay` 或 `./overlay/...`，并支持文件、子目录和整棵 overlay 目录挂载。
- 安装计划改为在应用 scope 下生成相对沙箱 source，移除 `METIS_DIR_*` 环境变量和宿主机绝对路径。
- 公开门禁规则与平台同步，补充路径越界、非规范化 source 和 overlay 根目录回归测试。

## 0.1.0

- 首次公开发布。
