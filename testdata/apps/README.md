# Metis MPK 契约 Fixture

`apps/<app-id>/` 保存可人工审计的 MPK v1 契约源文件。安装设置、环境、端口、挂载和能力都在 `manifest.yaml`；不再使用 `settings.yaml`。这里只覆盖 manifest、`compose.*.yaml` 和 overlay 等配置面，不携带 Docker 镜像归档。

构建并校验一个契约包：

```bash
make mpk-build MPK_APP=basic-app-a7x2m MPK_VERSION=1.0.0
```

`coverage-app-a7x2m` 是开发者文档驱动的全覆盖 fixture，覆盖双架构、全部能力、依赖、核心配置、三类模型插槽、图标、截图和 overlay。

`ContractOnly` 只属于测试和契约检查。商店上传、安装计划和 Agent 执行仍使用生产默认校验，必须携带真实镜像归档。
