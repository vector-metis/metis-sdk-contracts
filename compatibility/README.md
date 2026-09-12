# Runtime compatibility

SDK 实现应使用 `fixtures/runtime.json` 验证字段名、枚举投影、错误 reason 和缓存行为。平台运行时以 `/api/runtime/v1` 为稳定 HTTP seam；新增字段必须向后兼容，删除或改变既有字段需要新的 major 协议版本。

Browser SDK 只消费 `MetisBrowserSDK` 中声明的六个方法。Service endpoint、Master host/port 和应用 token 不属于浏览器契约。
