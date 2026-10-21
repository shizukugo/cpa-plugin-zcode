# ZCode CLIProxyAPI 插件

`zcode` 是一个 CLIProxyAPI 动态库插件，用于把 BigModel Coding Plan 上游接入 CPA，并以 Anthropic 格式提供静态模型执行器。

插件会注入 ZCode 桌面客户端指纹 headers，支持模型名映射和流式请求。它接收 Anthropic 格式 payload，其他 OpenAI / Responses / Claude 客户端协议由 CPA 的执行器管道统一转换。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    zcode:
      enabled: true
      priority: 1

      # 固定 BigModel API key；留空时透传 CPA 客户端请求的 API key
      api_key: ""

      base_url: "https://open.bigmodel.cn/api/anthropic"

      # 动态模型发现，默认 true
      dynamic_models: true

      # 模型列表缓存时间，单位秒，默认 300
      model_cache_seconds: 300

      # 兜底模型；动态发现关闭、api_key 为空或上游失败时使用
      models:
        - "GLM-5.3"
        - "GLM-5.3-Flash"

      # 客户端模型名 -> 上游模型名
      model_map: {}

      timeout_seconds: 300

      mimic:
        enabled: true
        app_version: "3.14.4"
        session_id: ""               # 留空：每个插件进程生成一次
        trace_id: ""                 # 留空：每个插件进程生成一次
        user_id: ""                  # 留空：从 API key 派生稳定 ID
        platform: "win32-x64"
        os_category: "windows"
        os_version: "10.0.19045"
        language: "zh-CN"
        timezone: "Asia/Shanghai"
        release_channel: "production"
        title: "Z Code@electron"
```

## 动态模型发现

动态模型发现默认开启。插件会携带 ZCode 指纹 headers 请求：

```text
GET <base_url>/v1/models
```

返回的模型 ID 会去空、去重后注册到 CPA，并按 `model_cache_seconds` 缓存，默认 300 秒。

以下情况会使用 `models` 兜底列表：

- `dynamic_models: false`
- `api_key` 为空
- 上游模型列表请求失败
- 上游返回为空或格式无效

CPA 静态模型注册阶段没有客户端 API key，因此需要动态发现时应配置固定 `api_key`。

## API key

配置 `api_key` 后，插件会固定使用该 BigModel credential 调用上游。

`api_key` 为空时，每个执行请求会从 CPA 客户端请求中提取：

- `Authorization: Bearer ...`
- 或 `x-api-key`

并透传给上游。

## 请求执行与路由

插件为已注册的模型声明自路由，将请求交给 ZCode 执行器，避免静态模型落入 CPA 的凭证选择流程而返回 `auth_not_found`。未匹配的模型交回 CPA 继续路由。

执行器支持 `execute`、`execute_stream` 和 `count_tokens`，分别调用上游 `/v1/messages` 和 `/v1/messages/count_tokens`；请求中的模型名按 `model_map` 映射。执行请求使用 CPA host HTTP callbacks，沿用 host 的取消、代理与请求日志机制，`timeout_seconds` 限制执行操作的持续时间。动态模型发现仍沿用原有模型列表请求实现。

上游 HTTP 错误保留状态码，错误文本中的本次上游 API key 会被脱敏。插件只构造上游所需 headers，不转发客户端 Cookie 等无关 headers；配置固定 `api_key` 时不会转发客户端认证凭证。

流式响应按 SSE 行输出，保留换行与空行，兼容原生 Anthropic 响应及 CPA 的协议转换。下游关闭或读取结束时释放 host HTTP stream 并取消操作。

本次修复不包含定时预热或额度重置功能。

## 构建与验证

```bash
make test
make vet
make build
make package
```

构建产物：

- Windows: `dist/zcode.dll`
- macOS: `dist/zcode.dylib`
- Linux / FreeBSD: `dist/zcode.so`

本地安装时，将动态库复制到 CPA 对应平台的插件目录，例如：

```bash
mkdir -p /path/to/CLIProxyAPI/plugins/windows/amd64
cp dist/zcode.dll /path/to/CLIProxyAPI/plugins/windows/amd64/zcode.dll
```

## 开源协议

MIT License，详见 [LICENSE](LICENSE)。

## 发布资产

每个 GitHub Release 包含：

```text
zcode_<version>_<goos>_<goarch>.zip
checksums.txt
```

每个 zip 的根目录中只包含对应平台的动态库。

## 兼容性

本插件实现 CPA v8 C ABI，依赖 `CLIProxyAPI/v8 v8.0.4`。

插件注册静态模型和 Anthropic 输入/输出格式执行器；其他客户端协议由 CPA 负责转换。
