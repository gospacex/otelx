package layer

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestBeginMarksErrorStatus 钉住 Begin 收尾闭包的错误标记语义。
//
// 这条用例是「补错误标记」那次修复的验收：修之前 span 句柄被封在闭包里，
// 调用方拿不到 span 去标状态，于是「SQL 失败」与「SQL 成功」在 Jaeger 上同色、
// 错误率恒为 0。四条断言分别对应四个容易回退的点：
//
//	① 非 nil error → span 状态 Error（并带 error 事件，Jaeger 才显示红）
//	② quiet 判定的错误（正常 miss）→ **不**标状态，不污染错误率
//	③ err=nil → 不标状态（成功路径）
//	④ traceOn=false → 空闭包，一个 span 都不产（开关仍然只控入链不控构造）
//
// 注入方式与各服埋点用例一致：Tracer() 向 otel 全局 provider 取，故这里
// otel.SetTracerProvider 注册内存 exporter 即可。
func TestBeginMarksErrorStatus(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)

	// ① 普通错误 → Error
	_, endErr := Begin(context.Background(), "repository", "product", "GetCategory", true, nil)
	endErr(errors.New("db down"))

	// ② quiet 命中的错误 → 不标状态
	_, endQuiet := Begin(context.Background(), "repository", "product", "GetProduct", true,
		func(err error) bool { return errors.Is(err, errQuiet) })
	endQuiet(errQuiet)

	// ③ 成功路径 → 不标状态
	_, endOK := Begin(context.Background(), "repository", "product", "ListProducts", true, nil)
	endOK(nil)

	// ④ traceOn=false → 空闭包，零 span
	_, endOff := Begin(context.Background(), "handler", "product", "CreateProduct", false, nil)
	endOff(errors.New("never recorded"))

	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("导出的 span 数 = %d，want 3（traceOn=false 不该产 span）", len(spans))
	}
	if got := spans[0].Status.Code; got != codes.Error {
		t.Errorf("① 普通错误：span 状态 = %v，want %v（失败必须在 Jaeger 上可见）", got, codes.Error)
	}
	if len(spans[0].Events) == 0 {
		t.Errorf("① 普通错误：span 上没有 error 事件，RecordError 没生效")
	}
	if got := spans[1].Status.Code; got == codes.Error {
		t.Errorf("② quiet 豁免的错误被标成了 %v：正常 miss 会顶高错误率", got)
	}
	if got := spans[2].Status.Code; got == codes.Error {
		t.Errorf("③ 成功路径被标成了 %v", got)
	}
	if name := spans[0].Name; name != "repository.product.GetCategory" {
		t.Errorf("span 名 = %q，want 三段式 repository.product.GetCategory", name)
	}
	if scope := spans[0].InstrumentationScope.Name; scope != TracerName {
		t.Errorf("instrumentation 名 = %q，want %q", scope, TracerName)
	}
}

// errQuiet 充当领域层的「正常未命中」错误（等价于各 repository.ErrNotFound）。
var errQuiet = errors.New("repository: record not found")
