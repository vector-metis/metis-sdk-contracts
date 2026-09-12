# Metis SDK Contracts

公开的 Metis 应用包和运行时契约，供 SDK、开发 CLI 和兼容性 fixture 使用。

本仓库不包含 Metis 平台服务端、在线应用商店服务端或生产配置。根目录 Go module 提供 CLI 所需的 MPK/manifest 校验能力；`proto/`、`browser/` 和 `fixtures/` 保存跨语言公开契约。

## 开发

```bash
go test ./...
```

Go module：

```text
github.com/vector-metis/metis-sdk-contracts
```

## 许可证

Apache-2.0，见 [LICENSE](LICENSE)。
