# Metis SDK Contracts

公开的 Metis 应用包和运行时契约，供 SDK、开发 CLI 和兼容性 fixture 使用。

本仓库不包含 Metis 平台服务端、在线应用商店服务端或生产配置。根目录 Go module 提供 CLI 所需的 MPK/manifest 校验能力；`proto/`、`browser/` 和 `fixtures/` 保存跨语言公开契约。

## Compose 运行约定

应用的常驻 Compose service 必须声明：

```yaml
restart: unless-stopped
```

一次性任务 service 可以使用 `x-metis.oneshot: true`，并省略 `restart` 或声明 `restart: no`。一次性任务不应使用其它重启策略。

当前 MPK v1 的路径契约要求：沙箱 source 在生成的 Compose 中使用应用 scope 下的相对目录
（`./program`、`./config`、`./data`、`./log`、`./tmp`），overlay 必须保留在 `overlay/` 层并使用
`./overlay` 或 `./overlay/...` 显式声明只读挂载。平台不再生成或注入 `METIS_DIR_*`；旧的 overlay
source 写法不会被自动改写，manifest 中的 `${METIS_DIR_*}` 占位符也会被门禁拒绝。

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
