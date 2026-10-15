// Package propagation 收口 OTel 全局传播器的读写（hubx 各层共用一份）。
//
// 为什么它是 otelx 的子包而不是根包：otelx 根包的定位是**装配**（把一份
// Config 变成可用的 TracerProvider 并注册全局），为此它依赖 configx、三个
// exporter 与 logx/zap。而传播器的读写只需要 otel 核心三项的十几行 —— 放进
// 根包会让只想要收发 traceparent 的组件（尤其 mqx 这类中间件 SDK）被迫链接
// 整条导出链出去。与 otelx/layer 同构，理由相同。
//
// 它**不**自己注册传播器：宿主（launchx 的 telemetry 步骤，或直接调用
// otelx.New）已经装好全局，本包只做读写。未注册时全局是 noop，注入为空串
// —— 这本身就是一条可断言的反向判据，不需要额外开关兜底。
//
// 存在原因（2026-10-12 收口）：同一段封装此前在 hubx 里存在三份 ——
// launchx.Inject/Extract、mqx.InjectTrace/ExtractTrace、examples/pkg/
// traceparent.Of。三份等价实现必须永远保持一致，而一致性只能靠人工同步，
// 迟早会漂（实测已经漂了：mall 四域用的常量名与 micro/mesh 不同，行为一致
// 纯属巧合）。收口之后，直接调用 otel 全局传播器的地方只有本包一处，
// 其余都是面向各自场景的**适配**（mqx 收 *Message.Headers，launchx 保留
// 库侧入口）。
package propagation

import (
	"context"

	"go.opentelemetry.io/otel"
	otelprop "go.opentelemetry.io/otel/propagation"
)

// Key W3C TraceContext 的头部键名。
//
// 取常量而不是各域手写字符串：它是跨进程续链的约定，写错不会有编译错误，
// 只会让链路静默断开。
const Key = "traceparent"

// Inject 把 ctx 里的 span context 注入出向请求的 carrier，完成跨进程续链。
//
// 入参取 TextMapCarrier 而不是 HTTP 头专用类型：HTTP 头是一类 carrier，
// MQ 消息头（MapCarrier / 自定义单值 map）是另一类。只认 HTTP 头会逼着
// 消息侧在业务代码里裸调 otel 全局，等于把唯一的续链入口堵死一半。
// carrier 为 nil 时静默返回（调用方不必先判空）。
func Inject(ctx context.Context, carrier otelprop.TextMapCarrier) {
	if carrier == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
}

// Extract 从入向 carrier 提取上游 span context，返回携带该 context 的 ctx。
//
// 与 Inject 成对提供：反向代理既要从入站请求取 context（Extract），也要把它
// 写进出站请求（Inject）。只做一半等于断链 —— 且表面上看两个服务"都有 span"，
// 极易被误判为链路正常。
// carrier 为 nil 时原样返回 ctx。
func Extract(ctx context.Context, carrier otelprop.TextMapCarrier) context.Context {
	if carrier == nil {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// Traceparent 取当前链路的 W3C traceparent 值；无活动 span 时返回空串。
func Traceparent(ctx context.Context) string {
	carrier := otelprop.MapCarrier(map[string]string{})
	Inject(ctx, carrier)
	return carrier.Get(Key)
}

// TraceparentIf 同 Traceparent，但 enabled 为假时直接返回空串。
//
// enabled 对应 traceflag 的调用链开关。这一层判断属于**取用方**的语义
// （要不要把链路 ID 带出去），所以以参数形式由调用方传入，而不是本包去读
// 开关 —— 传播 ≠ 埋点：关掉埋点不该让链路元数据也被造出来，但"要不要带"
// 是业务分层的决定。
//
// 关掉时若照样带出 traceparent，落库/落消息头的值会指向一条根本没有被采样
// 的 trace，排障时按它去 Jaeger 查会一无所获。
func TraceparentIf(ctx context.Context, enabled bool) string {
	if !enabled {
		return ""
	}
	return Traceparent(ctx)
}

// Fields 返回当前全局传播器会读写的 header 名集合。
//
// 用于验收断言：telemetry 步骤跑完后必须含 traceparent / tracestate / baggage
// （验收案例 T1）；为空即说明传播器没注册，跨服务链路必断。
func Fields() []string {
	return otel.GetTextMapPropagator().Fields()
}

// Propagator 返回当前全局传播器，供需要自定义 carrier 形态的调用方使用。
func Propagator() otelprop.TextMapPropagator {
	return otel.GetTextMapPropagator()
}
