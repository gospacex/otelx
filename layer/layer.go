// Package layer 业务侧分层埋点的极小封装（hubx 各服务共用）。
//
// 为什么它是 otelx 的子包而不是根包：otelx 根包的定位是**装配**（把一份
// Config 变成可用的 TracerProvider 并注册全局），为了做这件事它依赖
// configx、三个 exporter 与 logx/zap。而业务分层只需要「开一个 span、
// 收尾时按 err 标状态」这十几行 —— 放进根包会让只想要这个封装的服务
// 被迫链接整条导出链。子包只依赖 otel 核心三项。
//
// 它**不**自己 Init / 抢全局 TracerProvider：宿主（launchx 的 telemetry
// 步骤，或直接调用 otelx.New）已经注册好全局 provider 与传播器，业务再
// 抢一次全局位会让其余组件的 span 全部消失。本包与 dbx / mqx / rpcx 取
// tracer 的方式完全一致 —— 都向 otel 全局要，由装配层负责把全局装好。
//
// span 名字约定 "<layer>.<entity>.<op>"（例如 service.order.CreateOrder、
// messaging.outbox.send），跨服务、跨示例可比。
package layer

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TracerName 本包的 instrumentation 名（区别于 dbx / mqx / rpcx 各自的
// tracer 名）。取模块路径是 OTel 的通行约定，见 dbx 的 ormTracerName。
const TracerName = "github.com/gospacex/otelx/layer"

// End 分层 span 的收尾闭包：先按 err 标注 span 状态，再结束 span。
//
// 为什么收一个 err 而不是空参 end()：span 句柄被封在闭包里（业务分层因此
// 零 otel 依赖），代价是调用方也拿不到 span 去标错误 —— 于是「SQL 失败」与
// 「SQL 成功」在 Jaeger 上同色，错误率恒为 0，排障时看不出失败发生在哪一层。
// 收 err 把标记能力还回来，调用方仍只写一行：
//
//	defer func() { end(err) }()
//
// 前提是方法的结果列表已命名（Go 要求「全命名或全不命名」），这样 defer 里的
// err 能覆盖所有 return 路径 —— 包括直接返回字面量错误（return nil, fmt.Errorf(...)）
// 的路径，那些路径不会写进任何局部变量。
type End func(err error)

// Start 开一个 layerName.entity.op span。
//
// 返回的 span **必须**由调用方 End：丢弃句柄会让 span 永不导出，
// 且下游子 span 挂在悬空 parent 上（Jaeger 上表现为 missing-parent）。
// 只想「开完别忘关」的场景用 Begin 更省心。
func Start(ctx context.Context, layerName, entity, op string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, layerName+"."+entity+"."+op)
}

// Begin 开一个 layerName.entity.op span，返回收尾闭包（traceOn=false 时为空
// 闭包，调用方一律 defer 而不必写 if）。
//
// quiet 判定「不是故障」的错误（如持久层的 ErrNotFound —— 一次未命中是正常
// miss，不是错误）：命中时不改 span 状态，避免它污染错误率。nil 表示不豁免。
//
// 注意：err 必须是**接口意义上的**非空判断 —— 传 *codes.Error 这类具体指针时
// 先判空再进接口，否则 nil 指针装进 error 接口后 != nil，成功路径会被标成错误。
//
// traceOn 由调用方从配置取（各服 app.Setup 传的是 trace.enabled）。它管的是
// **业务分层 span** 入不入链；组件级 span（db.query / messaging.*）由各自组件
// 的 trace.on.db / trace.on.mq 开关裁决 —— 两套开关各管一段，不要互相顶替。
func Begin(ctx context.Context, layerName, entity, op string, traceOn bool, quiet func(error) bool) (context.Context, End) {
	if !traceOn {
		return ctx, func(error) {}
	}
	c, span := Start(ctx, layerName, entity, op)
	return c, func(err error) {
		if err != nil && (quiet == nil || !quiet(err)) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

// Attrs 在当前 span 上挂属性。
//
// span 句柄由收尾闭包持有（分层因此零 otel 依赖），挂属性只能经 ctx 取回 ——
// 这是「收尾闭包代标」这一选择的代价，收口在本包，免得每个示例、每一层各写
// 一份同款 helper。
//
// IsRecording 判空：埋点关闭时 SpanFromContext 返回的是 noop span，对它写属性
// 不会出错，只是白跑一次接口调用，判掉更干净。
func Attrs(ctx context.Context, kvs ...attribute.KeyValue) {
	if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
		span.SetAttributes(kvs...)
	}
}

// Tracer 返回本进程的业务 tracer。
//
// 每次都向全局 TracerProvider 现取，**不缓存成包级变量**：otel 的全局
// provider 是委托实现，测试里后注册的 provider 也能被先取到的 tracer 转发到
// （各服的埋点用例正是靠这一点注入内存 exporter）。
func Tracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer(TracerName)
}
