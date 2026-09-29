// otelx: hubx 生态的 OpenTelemetry 可观测性 SDK。
//
// 本模块是 hubx x 系列的**可观测性装配入口**：把一份 Config 变成可用的
// TracerProvider（含采样器与导出链路），注册为 OTel 全局 Provider，并对外
// 暴露 GetTracer / GetMeter 等零样板访问器。
//
// 与 traceflag 的分工：
//   - traceflag 管「开关」——各组件共享的调用链总闸与组件级开关；
//   - otelx 管「装配」——真正的 TracerProvider / Sampler / Exporter 构建。
//     traceflag 文档中说的「真正的闸门是宿主有没有注册 TracerProvider」，
//     这里的 New 就是那个注册动作。
package otelx

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gospacex/configx"
	_ "github.com/gospacex/logx/zap"
	"github.com/gospacex/traceflag"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

// shutdownTimeout 是 Close 时等待导出器冲刷缓冲 span 的上限。
const shutdownTimeout = 5 * time.Second

// Provider 可观测性提供者接口
type Provider interface {
	// TracerProvider 返回 tracer 提供者
	TracerProvider() trace.TracerProvider
	// MeterProvider 返回 meter 提供者
	MeterProvider() metric.MeterProvider
	// Close 关闭所有提供者
	Close() error
}

// Config 可观测性配置，同时也是配置文件中的 otelx 配置段。
//
// tag 说明：三套 tag 必须齐备，分别对应 configx 的三条读取通道——
//   - yaml：本地文件直读与文档示例
//   - json：远程配置中心（Nacos/Apollo）下发的 JSON
//   - mapstructure：configx 本地 viper 通道（viper.Sub(key).Unmarshal）
//
// 早期版本这三个结构体一个 tag 都没有，因此经 configx 读配置时
// service_name / tracer.enabled / sampler.ratio 等字段全部解码为零值：
// 「配了不生效」，且因为 FillDefaults 随后又补上默认值，故障表现为
// 静默的默认行为而非报错。字段名与 configx 的 otelx 段契约保持一致。
type Config struct {
	ServiceName string        `yaml:"service_name" json:"service_name" mapstructure:"service_name"`
	Tracer      TracerConfig  `yaml:"tracer" json:"tracer" mapstructure:"tracer"`
	Meter       MeterConfig   `yaml:"meter" json:"meter" mapstructure:"meter"`
	Sampler     SamplerConfig `yaml:"sampler" json:"sampler" mapstructure:"sampler"`
}

// TracerConfig Tracer 配置
type TracerConfig struct {
	// EnableTrace 统一调用链开关键（主键）。未配置时回退到 Enabled
	//（旧键 tracer.enabled，保留为兼容别名）；二者任一为 true 即启用。
	EnableTrace bool `yaml:"enable_trace" json:"enable_trace" mapstructure:"enable_trace"`
	// Enabled 是否启用 Tracer（旧键，兼容别名）
	Enabled  bool   `yaml:"enabled" json:"enabled" mapstructure:"enabled"`
	Exported string `yaml:"exported" json:"exported" mapstructure:"exported"` // otlp / stdout / none（jaeger 已停更，请改用 otlp）
	Endpoint string `yaml:"endpoint" json:"endpoint" mapstructure:"endpoint"` // OTLP/HTTP 端点，默认 localhost:4318
}

// MeterConfig MeterConfig
type MeterConfig struct {
	Enabled  bool `yaml:"enabled" json:"enabled" mapstructure:"enabled"`
	Interval int  `yaml:"interval" json:"interval" mapstructure:"interval"` // seconds
}

// SamplerConfig 采样配置
type SamplerConfig struct {
	Type   string  `yaml:"type" json:"type" mapstructure:"type"`       // traceidratio / parentbased / always_on / always_off
	Ratio  float64 `yaml:"ratio" json:"ratio" mapstructure:"ratio"`    // 0.0-1.0，仅 traceidratio 使用
	Parent bool    `yaml:"parent" json:"parent" mapstructure:"parent"` // 仅 parentbased 使用
}

// sectionKey otelx 在配置文件中的段名，与其余 x 系列 SDK 的取段方式一致。
const sectionKey = "otelx"

// FillDefaults 填充默认值
func (c *Config) FillDefaults() {
	if c.ServiceName == "" {
		c.ServiceName = "hubx-service"
	}
	if c.Sampler.Type == "" {
		c.Sampler.Type = "always_on"
	}
	if c.Tracer.Endpoint == "" {
		// 4318 是 OTLP/HTTP 的约定端口（4317 是 OTLP/gRPC）。
		// 本模块的导出器走 HTTP（otlptracehttp），默认值必须对齐 4318，
		// 否则按默认配置启动会静默连不上 collector。
		c.Tracer.Endpoint = "localhost:4318"
	}
}

// ===== Factory =====

// New 创建可观测性 Provider。
//
// 行为：
//  1. 填充默认值，接入 hubx 统一调用链开关（组件配置为 `enable_trace`
//     主键 / `tracer.enabled` 回退别名，全局总闸 traceflag.SetEnabled(false)
//     可一键熔断）；
//  2. Tracer 启用时构建真实 TracerProvider —— 按 SamplerConfig 选采样器、
//     按 TracerConfig.Exported/Endpoint 建导出器，并注册为 OTel 全局
//     Provider（这样 traceflag.Tracer 与各组件埋点才真正产 span）；
//  3. Tracer 未启用时返回 noop Provider，且不触碰全局状态。
func New(cfg *Config) (Provider, error) {
	cfg.FillDefaults()
	// 归一化：enable_trace 主键，tracer.enabled 回退别名
	cfg.Tracer.Enabled = cfg.Tracer.EnableTrace || cfg.Tracer.Enabled
	// 接入 hubx 统一调用链开关：组件自身配置为主开关，
	// 全局总闸 traceflag.SetEnabled(false) 可一键熔断。
	cfg.Tracer.Enabled = traceflag.Apply("otelx", cfg.Tracer.Enabled)
	return newProvider(cfg)
}

// GetTracer 便捷方法：获取全局 tracer
func GetTracer(name string) trace.Tracer {
	return otel.GetTracerProvider().Tracer(name)
}

// GetMeter 便捷方法：获取全局 meter
func GetMeter(name string) metric.Meter {
	return otel.GetMeterProvider().Meter(name)
}

// WithContext 将 TracerProvider 注入 context（供 middleware 使用）
func WithContext(ctx context.Context, tp trace.TracerProvider) context.Context {
	return context.WithValue(ctx, ctxKey{}, tp)
}

// GetFromContext 从 context 获取 TracerProvider
func GetFromContext(ctx context.Context) trace.TracerProvider {
	if v := ctx.Value(ctxKey{}); v != nil {
		if tp, ok := v.(trace.TracerProvider); ok {
			return tp
		}
	}
	return nil
}

type ctxKey struct{}

var _ Provider = (*provider)(nil)

type provider struct {
	tp trace.TracerProvider
	mp metric.MeterProvider

	// shutdown 非空表示本 Provider 拥有一个真实的 TracerProvider，
	// Close 时必须冲刷并释放它（否则尾部 span 会随进程退出而丢失）。
	shutdown  func(context.Context) error
	closeOnce sync.Once
	closeErr  error
}

// newProvider 按配置装配 OpenTelemetry Provider。
//
// 此前的实现直接透传 otel.GetTracerProvider()，导致 Config 里的
// Sampler / Exported / Endpoint / Meter.Enabled 四个字段虽然被反序列化、
// 被 FillDefaults 填了默认值，却从不被读取 —— 属于「配了不生效」的死配置。
// 现在这些字段都真正参与装配。
func newProvider(cfg *Config) (*provider, error) {
	// 关闭态：返回 noop，且不修改全局 Provider，避免影响同进程内的其他装配。
	if cfg == nil || !cfg.Tracer.Enabled {
		return &provider{
			tp: nooptrace.NewTracerProvider(),
			mp: otel.GetMeterProvider(),
		}, nil
	}

	ctx := context.Background()
	exp, err := buildExporter(ctx, cfg.Tracer)
	if err != nil {
		return nil, err
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(buildSampler(cfg.Sampler)),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", cfg.ServiceName),
		)),
	}
	if exp != nil {
		// 用 Batcher 而非 WithSyncer：逐条同步导出会把每个 span 都变成一次
		// 网络往返，高并发下直接把业务拖死。512/5s 是 OTel 的默认批量参数。
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)

	// Meter：本模块不内置 metric exporter（不暴露 Prometheus 端点，也不做
	// 指标聚合）。Meter.Enabled 为假时明确返回 noop，使调用方拿到的句柄
	// 从一开始就不产指标；为真时透传进程内全局 MeterProvider，由应用侧装配。
	mp := otel.GetMeterProvider()
	if !cfg.Meter.Enabled {
		mp = noopmetric.NewMeterProvider()
	}

	return &provider{tp: tp, mp: mp, shutdown: tp.Shutdown}, nil
}

// buildSampler 把配置映射为 OTel 采样器。
//
// Sampler.Type 取值（大小写不敏感）：traceidratio / parentbased /
// always_on / always_off。未知取值退化为 always_on，与 FillDefaults 的
// 默认值保持一致。
func buildSampler(c SamplerConfig) sdktrace.Sampler {
	ratio := clampRatio(c.Ratio)
	switch strings.ToLower(strings.TrimSpace(c.Type)) {
	case "traceidratio":
		if c.Parent {
			return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
		}
		return sdktrace.TraceIDRatioBased(ratio)
	case "parentbased":
		// Parent.Parent=true 时按比例抽样，否则沿用父 span 的采样决定。
		if c.Parent {
			return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
		}
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	case "always_off":
		return sdktrace.NeverSample()
	default: // always_on / "" / 未知取值
		return sdktrace.AlwaysSample()
	}
}

// clampRatio 把采样率夹到 [0,1]，避免配置写成 5 或 -1 时出现意外语义。
func clampRatio(r float64) float64 {
	switch {
	case r < 0:
		return 0
	case r > 1:
		return 1
	default:
		return r
	}
}

// buildExporter 按 TracerConfig.Exported 构建导出器。
//
// 返回 nil 表示「不导出」——此时 TracerProvider 仍然建立、span 全流程有效、
// 采样器照常工作，只是不落地。这在测试采样策略时很有用。
func buildExporter(ctx context.Context, c TracerConfig) (sdktrace.SpanExporter, error) {
	switch strings.ToLower(strings.TrimSpace(c.Exported)) {
	case "otlp":
		host, insecure := parseOTLPEndpoint(c.Endpoint)
		if host == "" {
			return nil, fmt.Errorf("otelx: exported=otlp 需要非空 endpoint")
		}
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(host)}
		if insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		return otlptracehttp.New(ctx, opts...)
	case "stdout":
		return stdouttrace.New(stdouttrace.WithPrettyPrint())
	case "jaeger":
		// go.opentelemetry.io/otel/exporters/jaeger 停留在 v1.17.0 且已停更，
		// 其依赖的 sdk API 与当前 otel v1.4x 不兼容。Jaeger 1.35+ 原生支持
		// OTLP，直接用 exported: otlp 指向 Jaeger 的 4318 端口即可。
		return nil, fmt.Errorf(
			"otelx: exported=%q 不再支持：Jaeger 自 1.35 起原生接收 OTLP，"+
				"请改用 exported: otlp 且 endpoint 形如 localhost:4318", c.Exported)
	case "", "none":
		return nil, nil
	default:
		return nil, fmt.Errorf(
			"otelx: exported=%q 不是受支持的导出类型（可选 otlp / stdout / none）", c.Exported)
	}
}

// parseOTLPEndpoint 拆掉 endpoint 上可能带的 scheme。
//
// otlptracehttp.WithEndpoint 只接受 host:port，带 scheme 会解析失败；
// 而配置文件里写 http:// 或 https:// 是很自然的写法，这里统一兜住。
// 无 scheme 时按明文处理（本地联调默认走 4317/4318 未加密端口）。
func parseOTLPEndpoint(ep string) (host string, insecure bool) {
	s := strings.TrimSpace(ep)
	switch {
	case strings.HasPrefix(s, "https://"):
		return strings.TrimPrefix(s, "https://"), false
	case strings.HasPrefix(s, "http://"):
		return strings.TrimPrefix(s, "http://"), true
	default:
		return s, true
	}
}

func (p *provider) TracerProvider() trace.TracerProvider { return p.tp }
func (p *provider) MeterProvider() metric.MeterProvider  { return p.mp }

// Close 冲刷并关闭本 Provider 持有的 TracerProvider。
//
// 幂等：重复调用只执行一次。关闭态（未启用 Tracer）的 Provider 没有
// 需要释放的资源，直接返回 nil。
func (p *provider) Close() error {
	if p.shutdown == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		p.closeErr = p.shutdown(ctx)
	})
	return p.closeErr
}

// ======================= ConfigX 集成 =======================
//
// 与其余 x 系列 SDK 保持同一套入口三件套：
//
//	ReadConfig        只读配置，不装配（预检/兼容用）
//	ReadAndCreate     读配置 → 取段 → New，返回可用句柄
//	MustReadAndCreate 同上，失败即退出进程

// ReadConfig 加载统一配置句柄（所有 x 系列 SDK 共享同一配置源）
func ReadConfig(opts ...configx.ConfigOptions) error {
	_, err := configx.ReadConfig(opts...)
	return err
}

// ReadAndCreate 从 configx 读取 otelx 配置并装配 Provider。
//
// 这一段必须存在："装配"才是本模块的价值。otelx 若只暴露 ReadConfig，
// 调用方拿到句柄后还得自己 Unmarshal、自己 New，等于把唯一的实际工作
// 留给了使用方。
//
// 配置示例：
//
//	otelx:
//	  service_name: myapp
//	  tracer:
//	    enabled: true
//	    exported: otlp        # otlp / stdout / none
//	    endpoint: localhost:4318
//	  sampler:
//	    type: parentbased
//	    ratio: 0.1
func ReadAndCreate(opts ...configx.ConfigOptions) (Provider, error) {
	c, err := configx.ReadConfig(opts...)
	if err != nil {
		return nil, fmt.Errorf("otelx: failed to read config: %w", err)
	}

	cfg, err := readSection(c)
	if err != nil {
		return nil, err
	}

	return New(cfg)
}

// readSection 从配置句柄取出 otelx 段。
//
// 单独成函数是为了可测：段解码依赖结构体的 mapstructure tag，而 tag 缺失
// 只会表现为"字段静默为零值"，无法从 Provider 的行为上观察出来，只能直接
// 断言解出来的 Config。详见 Config 的 tag 说明。
func readSection(c configx.Config) (*Config, error) {
	cfg := &Config{}
	if err := c.Unmarshal(sectionKey, cfg); err != nil {
		return nil, fmt.Errorf("otelx: failed to unmarshal %s section: %w", sectionKey, err)
	}
	return cfg, nil
}

// MustReadAndCreate 类似 ReadAndCreate，失败时退出进程
func MustReadAndCreate(opts ...configx.ConfigOptions) Provider {
	p, err := ReadAndCreate(opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "otelx: failed to initialize provider: %v\n", err)
		os.Exit(1)
	}
	return p
}
