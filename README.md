<div align="center">

# otelx

**hubx x 系列可观测性 SDK：统一的 OpenTelemetry 装配与获取入口**

The observability assembly entry point for the hubx ecosystem — one `Config` in, a wired `TracerProvider` out.

[![Go](https://img.shields.io/badge/Go-1.25.0-00ADD8?logo=go&logoColor=white)](https://go.dev/)
![License](https://img.shields.io/badge/License-MIT-green.svg)
[![Release](https://img.shields.io/badge/release-v0.0.1-blue.svg)](https://github.com/gospacex/otelx/tags)

</div>

---

## 目录

- [1. 项目定位](#1-项目定位)
- [2. 核心特性](#2-核心特性)
- [3. 架构](#3-架构)
- [4. 快速开始](#4-快速开始)
- [5. 配置参考](#5-配置参考)
- [6. API 参考](#6-api-参考)
- [7. 本地可观测栈（docker-compose）](#7-本地可观测栈docker-compose)
- [8. 在 hubx 生态中的位置](#8-在-hubx-生态中的位置)
- [9. 测试与质量](#9-测试与质量)
- [10. 版本与兼容性](#10-版本与兼容性)
- [11. 许可证](#11-许可证)

## 1. 项目定位

**解决什么问题**

- 把一份 `Config` 变成**真正可用的** `TracerProvider`：按 `SamplerConfig` 选采样器、按 `TracerConfig.Exported` / `Endpoint` 建导出器、按 `ServiceName` 建 Resource，然后注册为 OTel 全局 Provider。
- 收敛 Tracer / Meter / Sampler 参数到结构化配置，并用 `FillDefaults` 兜底。
- 提供 `GetTracer` / `GetMeter` 零样板访问器，以及 `WithContext` / `GetFromContext` 供中间件传递 `TracerProvider`。
- 通过 `ReadConfig` 接入所有 x 系列 SDK 共享的 `configx` 配置源。

**不解决什么问题（能力边界）**

- **不提供 metric 导出链路**。本模块不含任何 metric exporter，也不暴露 Prometheus 端点、不做指标聚合。`Meter.Enabled` 只决定 `MeterProvider()` 返回真实全局句柄还是 noop；`Meter.Interval` 当前**未被消费**（见 [§5](#5-配置参考)）。
- **不提供日志实现**。日志归 `logx`；otelx 仅空白导入 `logx/zap` 完成 zap provider 注册。
- **不提供配置文件键名映射**。`Config` 系列结构体未定义 `yaml` / `json` tag，本模块不声明任何配置文件键名；如需从文件加载，由调用方借助 `configx` 自行反序列化。
- **不做跨模块的开关治理**。开关（调用链总闸、组件级开关）归 `traceflag`，otelx 只负责在最外层做一次收敛（见 [§8](#8-在-hubx-生态中的位置)）。

**与同系列 SDK 的关系**

| 模块 | 职责 | 与 otelx 的关系 |
|---|---|---|
| `configx` | 统一配置读取（本地文件 / Nacos / Apollo） | otelx 的 `ReadConfig` 直接委托给 `configx.ReadConfig` |
| `logx` | 统一日志（Zap），支持 Span Event 集成 | otelx 空白导入 `logx/zap` 完成 provider 注册 |
| `traceflag` | 各组件共享的调用链**开关**（总闸 + 组件级） | `New` 内部用 `traceflag.Apply("otelx", ...)` 收敛开关 |
| `otelx` | OpenTelemetry **装配**（Sampler / Exporter / Provider） | 本模块 |

## 2. 核心特性

- **真实装配**：`New` 会构建 `sdktrace.TracerProvider` 并注册为全局 Provider —— 这是让全仓埋点真正产出 span 的那一步。
- **采样策略可配**：`Sampler.Type` 支持 `traceidratio` / `parentbased` / `always_on` / `always_off`（大小写不敏感），`Ratio` 自动夹到 `[0,1]`。
- **导出目标可配**：`Exported` 支持 `otlp`（OTLP/HTTP）与 `stdout`；`jaeger` 会给出明确的迁移指引而非静默失败。
- **端点多写法兼容**：`Endpoint` 允许带或不带 `http://` / `https://` 前缀，带 `https://` 时自动启用 TLS。
- **批量导出**：使用 `WithBatcher`（512 / 5s），而非逐条同步导出。
- **优雅关闭**：`Close()` 冲刷缓冲 span 并释放 Provider，幂等且带 5s 超时。
- **关闭态零副作用**：`Tracer.Enabled` 为假时返回 noop Provider，且**不修改**全局状态，不影响同进程内的其他装配。
- **统一开关**：`traceflag.SetEnabled(false)` 可一键熔断本模块埋点。
- **本地可观测栈开箱**：`docker-compose.yml` 提供 Jaeger / Prometheus / Grafana / Loki 单机环境。

## 3. 架构

```
业务代码
   ├─ otelx.New(cfg)
   │     ├─ cfg.FillDefaults()
   │     ├─ traceflag.Apply("otelx", enabled)      # 开关收敛
   │     └─ newProvider(cfg)
   │           ├─ buildSampler(cfg.Sampler)        -> sdktrace.Sampler
   │           ├─ buildExporter(cfg.Tracer)        -> otlptracehttp / stdout / nil
   │           ├─ resource.NewSchemaless(service.name)
   │           ├─ sdktrace.NewTracerProvider(..., WithBatcher(exp))
   │           └─ otel.SetTracerProvider(tp)       # 全局注册（关键）
   │
   ├─ otelx.GetTracer(name) ─► otel.GetTracerProvider().Tracer(name)
   ├─ otelx.GetMeter(name)  ─► otel.GetMeterProvider().Meter(name)
   ├─ otelx.WithContext(ctx, tp) ─► otelx.GetFromContext(ctx)
   └─ otelx.ReadConfig(...) ─► configx.ReadConfig(...)

包级副作用：import _ "logx/zap" ─► logx.RegisterProvider(...)
```

各组件（cachex / dbx / kvx / …）通过 `traceflag.Tracer` 埋点，`Start` 时惰性读取
`otel.GetTracerProvider()`。**如果宿主没有调用 `otelx.New`，全局仍是 OTel 默认的 noop
Provider，所有 span 都是空操作。** 这就是 otelx 存在的意义。

## 4. 快速开始

### 4.1 安装

```bash
go get github.com/gospacex/otelx
```

要求 Go >= 1.25.0（见 `go.mod`）。直接依赖 `go.opentelemetry.io/otel` v1.46.0 及其
`sdk` / `metric` / `trace` / `exporters/*` 子模块；`configx` / `logx` / `traceflag` 为同系列
本地模块（`go.mod` 中以 `replace` 指向同级目录）。

### 4.2 最小可用示例

```go
package main

import (
	"context"
	"fmt"

	"github.com/gospacex/otelx"
)

func main() {
	// Tracer.Enabled=false 时返回 noop Provider —— 进程能跑，只是不埋点。
	p, err := otelx.New(&otelx.Config{ServiceName: "demo"})
	if err != nil {
		panic(err)
	}
	defer p.Close()

	tr := otelx.GetTracer("demo")
	_, span := tr.Start(context.Background(), "hello")
	defer span.End()

	fmt.Println(p.TracerProvider() != nil, span.SpanContext().IsValid())
}
```

### 4.3 接 OTLP（推荐）

```go
p, err := otelx.New(&otelx.Config{
	ServiceName: "order-service",
	Tracer: otelx.TracerConfig{
		Enabled:  true,
		Exported: "otlp",
		Endpoint: "localhost:4318", // OTLP/HTTP；写 https://host:4318 会自动启用 TLS
	},
	Sampler: otelx.SamplerConfig{
		Type:   "parentbased", // 有父 span 时遵循上游采样决定
		Ratio:  0.1,           // 无父 span 时按 10% 抽样
		Parent: true,
	},
	Meter: otelx.MeterConfig{Enabled: false},
})
if err != nil {
	panic(err)
}
defer p.Close() // 冲刷缓冲 span
```

### 4.4 从配置文件加载

```go
import (
	"github.com/gospacex/configx"
	"github.com/gospacex/otelx"
)

// otelx 自身不做键名映射，先由 configx 读出再转成结构体
if err := otelx.ReadConfig(configx.ConfigOptions{ConfigFile: "config.yaml"}); err != nil {
	panic(err)
}
// cfg := ... // 从 configx 句柄反序列化到 otelx.Config
// p, _ := otelx.New(cfg)
```

## 5. 配置参考

### 5.1 字段与生效状态

| 字段 | 类型 | `FillDefaults` 默认值 | 是否被消费 | 说明 |
|---|---|---|---|---|
| `ServiceName` | `string` | `hubx-service` | ✅ | 写入 Resource 的 `service.name` |
| `Tracer.Enabled` | `bool` | `false`（零值） | ✅ | 主开关，与 `traceflag` 总闸取 AND |
| `Tracer.Exported` | `string` | `""` | ✅ | `otlp` / `stdout` / `none`；`jaeger` 报错并给迁移指引 |
| `Tracer.Endpoint` | `string` | `localhost:4318` | ✅ | 仅 `otlp` 使用；可带 `http://` / `https://` |
| `Meter.Enabled` | `bool` | `false`（零值） | ✅ | 假时 `MeterProvider()` 返回 noop；真时透传全局 |
| `Meter.Interval` | `int` | `0`（零值） | ❌ | **当前未被消费**：本模块无 metric exporter，该字段为将来预留 |
| `Sampler.Type` | `string` | `always_on` | ✅ | 见 [§5.2](#52-sampler-映射) |
| `Sampler.Ratio` | `float64` | `0`（零值） | ✅ | 仅比率类采样器使用，自动夹到 `[0,1]` |
| `Sampler.Parent` | `bool` | `false`（零值） | ✅ | 决定 `TraceIDRatioBased` 是否外包一层 `ParentBased` |

> `FillDefaults` 只对 `ServiceName` / `Sampler.Type` / `Tracer.Endpoint` 三个空值兜底，
> 其余字段保持零值。已显式设置的字段不会被覆盖。

### 5.2 Sampler 映射

| `Sampler.Type` | `Parent` | 实际采样器 |
|---|---|---|
| `always_on`（默认）、空、未知取值 | — | `AlwaysSample` |
| `always_off` | — | `NeverSample` |
| `traceidratio` | `false` | `TraceIDRatioBased(Ratio)` |
| `traceidratio` | `true` | `ParentBased(TraceIDRatioBased(Ratio))` |
| `parentbased` | `false` | `ParentBased(AlwaysSample)` |
| `parentbased` | `true` | `ParentBased(TraceIDRatioBased(Ratio))` |

`Type` 大小写不敏感、忽略首尾空白；未知取值退化为 `always_on`（与该字段的默认值一致）。

### 5.3 Exporter 支持

| `Exported` | 行为 |
|---|---|
| `otlp` | OTLP/HTTP 导出器。`Endpoint` 无 scheme 或 `http://` 时走明文；`https://` 时启用 TLS |
| `stdout` | 打印到标准输出，本地调试用 |
| `""` / `none` | **不导出**：Provider 照常建立、采样器照常工作、span 有效，只是不落地 |
| `jaeger` | **报错**。`exporters/jaeger` 停留在 v1.17.0 且已停更，与当前 otel v1.4x 不兼容；Jaeger ≥1.35 原生接收 OTLP，请改用 `otlp` + 4318 |
| 其他 | **报错**，错误信息包含非法取值 |

### 5.4 一个已修正的默认值陷阱

旧版 `FillDefaults` 把 `Endpoint` 默认成 `localhost:4317`。4317 是 **OTLP/gRPC** 端口，
而本模块的导出器走 **HTTP**（4318）—— 按默认配置启动会静默连不上 collector。
现已改为 `localhost:4318`。若你的 Jaeger 只开了 gRPC，请显式配置端口。

## 6. API 参考

### 6.1 类型

```go
// Provider 可观测性提供者接口
type Provider interface {
	TracerProvider() trace.TracerProvider
	MeterProvider() metric.MeterProvider
	Close() error
}

// Config 可观测性配置
type Config struct {
	ServiceName string
	Tracer      TracerConfig
	Meter       MeterConfig
	Sampler     SamplerConfig
}

// TracerConfig Tracer 配置
type TracerConfig struct {
	Enabled  bool
	Exported string // otlp / stdout（jaeger 已停更，请改用 otlp）
	Endpoint string
}

// MeterConfig MeterConfig
type MeterConfig struct {
	Enabled  bool
	Interval int // seconds（当前未被消费）
}

// SamplerConfig 采样配置
type SamplerConfig struct {
	Type   string  // traceidratio / parentbased / always_on / always_off
	Ratio  float64 // 0.0-1.0，仅 traceidratio 使用
	Parent bool    // 仅 parentbased 使用
}
```

### 6.2 函数与方法

| 符号 | 签名 | 说明 |
|---|---|---|
| `New` | `func New(cfg *Config) (Provider, error)` | 填充默认值 → 收敛开关 → 装配 Provider；启用时注册全局 |
| `GetTracer` | `func GetTracer(name string) trace.Tracer` | `otel.GetTracerProvider().Tracer(name)` |
| `GetMeter` | `func GetMeter(name string) metric.Meter` | `otel.GetMeterProvider().Meter(name)` |
| `WithContext` | `func WithContext(ctx, tp) context.Context` | 把 `TracerProvider` 注入 context（供中间件使用） |
| `GetFromContext` | `func GetFromContext(ctx) trace.TracerProvider` | 取回；不存在时返回 `nil` |
| `ReadConfig` | `func ReadConfig(opts ...configx.ConfigOptions) error` | 委托 `configx.ReadConfig`；只解析配置，不装配 |
| `ReadAndCreate` | `func ReadAndCreate(opts ...configx.ConfigOptions) (Provider, error)` | 读配置 → 取 `otelx` 段 → `New`。与其余 x 系列 SDK 同一套入口 |
| `MustReadAndCreate` | `func MustReadAndCreate(opts ...configx.ConfigOptions) Provider` | 同上，失败写 stderr 并 `os.Exit(1)` |
| `ReadSection`（私有） | `func readSection(c configx.Config) (*Config, error)` | 取段单独成函数以便断言：tag 缺失只会静默归零，无法从 Provider 行为观察 |
| `(*Config).FillDefaults` | `func (c *Config) FillDefaults()` | 填充 `ServiceName` / `Sampler.Type` / `Tracer.Endpoint` |
| `(Provider).Close` | `func (p *provider) Close() error` | 冲刷并关闭；幂等，超时 5s |

**`ReadAndCreate` 的最小配置**

```yaml
otelx:
  service_name: myapp
  tracer:
    enabled: true
    exported: otlp        # otlp / stdout / none
    endpoint: localhost:4318
  sampler:
    type: parentbased
    ratio: 0.1
```

```go
p := otelx.MustReadAndCreate(configx.ConfigOptions{ConfigFile: "config.yaml"})
defer p.Close()   // 冲刷缓冲 span
```

> `Config` / `TracerConfig` / `MeterConfig` / `SamplerConfig` 原先**一个序列化 tag 都没有**，
> 经 configx 读取时所有字段都解码为零值，而 `FillDefaults` 紧接着又把零值补成默认值——
> 表现为"配置写了但不生效、还不报错"。现三套 tag（yaml/json/mapstructure）齐备，
> 并由 `TestReadSection_DecodesEveryField` 守住。

**`New` 的行为分支**

1. `cfg == nil` 或 `Tracer.Enabled == false` → 返回 noop Provider，**不触碰全局状态**，`Close` 为 no-op。
2. `Tracer.Enabled == true` → 建导出器（失败则返回错误，全局不受影响）→ 建 `TracerProvider`
   → `otel.SetTracerProvider(tp)` → `Close` 时 `tp.Shutdown`。

包内私有：`provider`、`ctxKey`、`buildSampler`、`buildExporter`、`parseOTLPEndpoint`、`clampRatio`。

## 7. 本地可观测栈（docker-compose）

模块根目录的 `docker-compose.yml` 提供单机开发用可观测栈（网络名 `hubx-otelx-net`，日志驱动 `json-file`，单文件 10m × 3）。

### 7.1 服务与端口

| 服务 | 镜像 tag | 容器名 | 暴露端口 | 用途 |
|---|---|---|---|---|
| `jaeger` | `jaegertracing/all-in-one:1.63.0` | `hubx-jaeger` | `16686`（UI）、`14268`、`14250`、`6831/udp`、`4317`（OTLP gRPC）、`4318`（OTLP HTTP） | Trace 查询与 OTLP 接收 |
| `prometheus` | `prom/prometheus:v3.2.1` | `hubx-prometheus` | `9090` | 指标存储与查询 |
| `grafana` | `grafana/grafana:11.6.0` | `hubx-grafana` | `3000` | 可视化（`admin` / `admin`） |
| `loki` | `grafana/loki:3.3.2` | `hubx-loki` | `3100` | 日志聚合 |

### 7.2 启动

```bash
docker compose up -d
```

> **对齐提示**：`Tracer.Endpoint` 默认值 `localhost:4318` 对应上表 Jaeger 的 **OTLP/HTTP** 端口，
> 开箱即可联调。若写 `localhost:4317` 则需改用 gRPC 导出器（本模块未内置）。

> compose 以 bind mount 引用了 `./prometheus/config/prometheus.yml`、`./grafana/config/datasources.yml`、
> `./loki/config/loki-config.yml`，当前仓库未包含这三个目录，按需自行补充后再启动。

## 8. 在 hubx 生态中的位置

hubx 的调用链被拆成两层，职责不重叠：

| 层 | 模块 | 管什么 |
|---|---|---|
| 开关层 | `traceflag` | 全局总闸 + 组件级开关，决定「埋不埋」 |
| 装配层 | **`otelx`** | Sampler / Exporter / TracerProvider，决定「埋到哪里」 |

`traceflag` 的设计文档里有一句关键的话：*「真正的闸门是宿主有没有注册 TracerProvider」*。
`otelx.New` 就是这个注册动作。两者组合后的完整链路：

```
宿主机 main()  →  otelx.New(cfg)  →  otel.SetTracerProvider(tp)     # 装配
组件初始化      →  traceflag.Apply("kvx", cfg.EnableTrace)          # 开关
组件埋点        →  tracer.Start(ctx, "Get")  →  otel.GetTracerProvider()  # 落到 real provider
全局熔断        →  traceflag.SetEnabled(false)                       # 一刀切
```

**当前调用方**（`grep -rn "gospacex/otelx" --include='*.go'`）：

| 调用方 | 位置 | 形式 | 说明 |
|---|---|---|---|
| `micro_apps` | `micro_apps/tools/tools.go` | `_ "github.com/gospacex/otelx"` | 空白导入，锁定依赖图 |
| `micro_apps` | `micro_apps/go.mod` | `replace ... => ../otelx` | 本地工作树引用 |

> `HUBX-FUNCTION-MATRIX.md` 中 otelx 登记为「可观测性基础设施，无需独立示例」。
> 除上述空白导入外，尚无兄弟模块调用 `otelx.New`；**这意味着当前全仓埋点在运行期
> 仍落到 OTel 默认 noop Provider。** 要让埋点真正生效，宿主必须显式调用 `otelx.New`。

## 9. 测试与质量

模块自带 `interface_test.go`（15 个用例，覆盖采样器映射、端点解析、开关分支、错误分支与 `Close` 幂等）：

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
```

三项均已在本仓库执行通过。测试不依赖任何外部中间件（`stdout` 导出器直接输出到控制台）。

## 10. 版本与兼容性

- **Go 版本**：`go 1.25.0`（取自 `go.mod`）
- **当前版本**：v0.0.1
- **直接依赖**：`go.opentelemetry.io/otel` v1.46.0（含 `sdk` / `metric` / `trace` / `exporters/otlp/otlptrace/otlptracehttp` / `exporters/stdout/stdouttrace`）、`github.com/gospacex/configx`、`github.com/gospacex/logx`、`github.com/gospacex/traceflag`
- **本地开发**：`go.mod` 内 `replace` 将上述三个同系列模块指向 `../configx`、`../logx`、`../traceflag`
- **已知限制**：
  - 不内置 metric 导出链路，`Meter.Interval` 未被消费；
  - 不支持 Jaeger 原生 exporter（已停更），请走 OTLP；
  - 未做 Zipkin 导出；
  - 多次调用 `New` 会多次覆盖全局 Provider（后建者生效），调用方应保证单次装配。

## 11. 许可证

MIT License
