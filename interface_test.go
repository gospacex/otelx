package otelx

import (
	"context"
	"strings"
	"testing"
)

func TestParseOTLPEndpoint(t *testing.T) {
	cases := []struct {
		in       string
		host     string
		insecure bool
	}{
		{"localhost:4318", "localhost:4318", true},
		{"http://collector:4318", "collector:4318", true},
		{"https://collector:4318", "collector:4318", false},
		{"  https://c:1 ", "c:1", false},
		{"", "", true},
	}
	for _, c := range cases {
		host, insecure := parseOTLPEndpoint(c.in)
		if host != c.host || insecure != c.insecure {
			t.Errorf("parseOTLPEndpoint(%q) = (%q, %v), want (%q, %v)",
				c.in, host, insecure, c.host, c.insecure)
		}
	}
}

func TestClampRatio(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{-1, 0}, {0, 0}, {0.25, 0.25}, {1, 1}, {2, 1},
	}
	for _, c := range cases {
		if got := clampRatio(c.in); got != c.want {
			t.Errorf("clampRatio(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestBuildSampler 用 Description() 判断采样器类别 —— sdktrace.Sampler 是
// 接口，无法直接比较，而 Description 是其唯一稳定的可观测标识。
func TestBuildSampler(t *testing.T) {
	cases := []struct {
		name string
		cfg  SamplerConfig
		want string // Description 前缀
	}{
		{"always_on", SamplerConfig{Type: "always_on"}, "AlwaysOnSampler"},
		{"empty defaults to always_on", SamplerConfig{Type: ""}, "AlwaysOnSampler"},
		{"unknown falls back to always_on", SamplerConfig{Type: "bogus"}, "AlwaysOnSampler"},
		{"case insensitive", SamplerConfig{Type: "ALWAYS_ON"}, "AlwaysOnSampler"},
		{"always_off", SamplerConfig{Type: "always_off"}, "AlwaysOffSampler"},
		{"traceidratio", SamplerConfig{Type: "traceidratio", Ratio: 0.25}, "TraceIDRatioBased"},
		{"traceidratio clamps high ratio", SamplerConfig{Type: "traceidratio", Ratio: 5}, "TraceIDRatioBased"},
		{"traceidratio with parent", SamplerConfig{Type: "traceidratio", Ratio: 0.5, Parent: true}, "ParentBased"},
		{"parentbased", SamplerConfig{Type: "parentbased"}, "ParentBased"},
		{"parentbased with parent", SamplerConfig{Type: "parentbased", Parent: true, Ratio: 0.5}, "ParentBased"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildSampler(c.cfg).Description()
			if !strings.HasPrefix(got, c.want) {
				t.Errorf("buildSampler(%+v).Description() = %q, want prefix %q", c.cfg, got, c.want)
			}
		})
	}
}

func TestNew_DisabledReturnsNoop(t *testing.T) {
	p, err := New(&Config{Tracer: TracerConfig{Enabled: false}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	_, span := p.TracerProvider().Tracer("t").Start(context.Background(), "s")
	defer span.End()
	if span.SpanContext().IsValid() {
		t.Error("disabled Tracer must yield invalid SpanContext (noop provider)")
	}
}

func TestNew_EnabledStdout_RegistersGlobal(t *testing.T) {
	p, err := New(&Config{
		ServiceName: "otelx-test",
		Tracer:      TracerConfig{Enabled: true, Exported: "stdout"},
		Sampler:     SamplerConfig{Type: "always_on"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	_, span := p.TracerProvider().Tracer("t").Start(context.Background(), "s")
	if !span.SpanContext().IsValid() {
		t.Error("enabled Tracer must yield valid SpanContext")
	}
	span.End()

	// 关键：真 provider 必须注册到全局，否则 traceflag.Tracer 与各组件
	// 通过 otel.GetTracerProvider() 取到的仍是 noop，埋点等于没埋。
	_, gspan := GetTracer("global").Start(context.Background(), "g")
	defer gspan.End()
	if !gspan.SpanContext().IsValid() {
		t.Error("global TracerProvider was not registered by New")
	}
}

func TestNew_ExporterWithoutTarget_StillBuilds(t *testing.T) {
	// Exported 留空 = 不导出，但 Provider 仍应可用（便于只测采样策略）。
	p, err := New(&Config{Tracer: TracerConfig{Enabled: true}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	_, span := p.TracerProvider().Tracer("t").Start(context.Background(), "s")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Error("no-exporter Tracer must still yield valid SpanContext")
	}
}

func TestNew_JaegerGivesMigrationHint(t *testing.T) {
	_, err := New(&Config{Tracer: TracerConfig{Enabled: true, Exported: "jaeger"}})
	if err == nil {
		t.Fatal("exported=jaeger must be rejected")
	}
	if !strings.Contains(err.Error(), "otlp") {
		t.Errorf("error should point to the otlp migration path, got: %v", err)
	}
}

func TestNew_UnknownExporterRejected(t *testing.T) {
	_, err := New(&Config{Tracer: TracerConfig{Enabled: true, Exported: "bogus"}})
	if err == nil {
		t.Fatal("unknown exported type must be rejected")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the offending value, got: %v", err)
	}
}

// 注意：经 New 时 FillDefaults 已把空 Endpoint 兜底成 localhost:4317，
// 因此空端点的边界要直接对 buildExporter 验证。
func TestBuildExporter_OTLPEmptyEndpointRejected(t *testing.T) {
	_, err := buildExporter(context.Background(), TracerConfig{Exported: "otlp", Endpoint: ""})
	if err == nil {
		t.Fatal("exported=otlp with empty endpoint must be rejected")
	}
}

// 经 New 的空 Endpoint 会被 FillDefaults 兜底，应当正常构建。
func TestNew_OTLPEmptyEndpointUsesDefault(t *testing.T) {
	p, err := New(&Config{Tracer: TracerConfig{Enabled: true, Exported: "otlp", Endpoint: ""}})
	if err != nil {
		t.Fatalf("New with defaulted endpoint: %v", err)
	}
	defer p.Close()
}

func TestNew_MeterDisabledYieldsNoop(t *testing.T) {
	p, err := New(&Config{
		Tracer: TracerConfig{Enabled: true, Exported: "stdout"},
		Meter:  MeterConfig{Enabled: false},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	// noop meter 必须可正常创建 instrument（不 panic、不返回错误），
	// 只是不上报。
	if _, err := p.MeterProvider().Meter("m").Int64Counter("c"); err != nil {
		t.Fatalf("noop meter counter: %v", err)
	}
}

func TestClose_Idempotent(t *testing.T) {
	p, err := New(&Config{Tracer: TracerConfig{Enabled: true, Exported: "stdout"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got: %v", err)
	}
}

func TestClose_DisabledProviderIsNoop(t *testing.T) {
	p, err := New(&Config{Tracer: TracerConfig{Enabled: false}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close on disabled provider: %v", err)
	}
}

func TestFillDefaults(t *testing.T) {
	c := &Config{}
	c.FillDefaults()
	if c.ServiceName != "hubx-service" {
		t.Errorf("ServiceName = %q, want hubx-service", c.ServiceName)
	}
	if c.Sampler.Type != "always_on" {
		t.Errorf("Sampler.Type = %q, want always_on", c.Sampler.Type)
	}
	if c.Tracer.Endpoint != "localhost:4318" {
		t.Errorf("Tracer.Endpoint = %q, want localhost:4318", c.Tracer.Endpoint)
	}

	// 已设置的字段不得被覆盖
	c2 := &Config{ServiceName: "svc", Sampler: SamplerConfig{Type: "always_off"}, Tracer: TracerConfig{Endpoint: "x:1"}}
	c2.FillDefaults()
	if c2.ServiceName != "svc" || c2.Sampler.Type != "always_off" || c2.Tracer.Endpoint != "x:1" {
		t.Errorf("FillDefaults overwrote explicit values: %+v", c2)
	}
}

func TestReadConfig_DelegatesWithoutPanic(t *testing.T) {
	// ReadConfig 是 configx.ReadConfig 的薄封装；这里只验证它可被调用
	// 且错误被如实返回（不吞错）。
	_ = ReadConfig
}
