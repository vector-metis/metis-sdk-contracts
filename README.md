# Metis SDK Contracts

公开的 Metis 应用包和运行时契约，供 SDK、开发 CLI 和兼容性 fixture 使用。

本仓库不包含 Metis 平台服务端、在线应用商店服务端或生产配置。根目录 Go module 提供 CLI 所需的 MPK/manifest 校验能力；`proto/`、`browser/` 和 `fixtures/` 保存跨语言公开契约。

## Manifest 与 Compose 运行约定

常驻 service 必须在 manifest 中声明生命周期；平台随后在最终 Compose 中生成 Docker 所需的重启策略：

```yaml
services:
  web:
    lifecycle: {restart: unless-stopped}
```

一次性 service 使用 `lifecycle.oneshot: true`，可省略 `restart` 或写成 `no`。源 Compose 不得声明
`restart` 或标准 `x-metis`，也不能把生命周期策略藏在 Compose 扩展字段中。

挂载使用 `source + subpath`：`source` 只能是 `program`、`config`、`data`、`log`、`tmp` 或 `overlay`，
`subpath` 是该逻辑根目录内的规范化相对路径。例如：

```yaml
services:
  web:
    mounts:
      - {source: data, subpath: postgres, target: /var/lib/postgresql/data}
      - {source: overlay, subpath: nginx.conf, target: /etc/nginx/nginx.conf, read_only: true}
```

平台最终在应用 scope 下展开为 `./data/postgres` 和 `./overlay/nginx.conf`，应用不需要知道 Worker 的宿主路径。
镜像 `Config.Volumes` 中的每个 target 也必须在 manifest 中显式绑定到可写的 managed mount；否则 `validate`/`pack`
会以 `MPK-VOLUME-UNMANAGED` 拒绝包，避免 Docker 创建平台无法备份和迁移的匿名 volume。平台不生成或注入
`METIS_DIR_*`，旧的完整路径 source 不会自动改写。

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
