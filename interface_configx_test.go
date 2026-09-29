package otelx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gospacex/configx"
)

// writeConfig 落一份临时 config.yaml 并返回其路径。
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

const fullSection = `
otelx:
  service_name: myapp
  tracer:
    enabled: true
    exported: none
    endpoint: collector.internal:4318
  meter:
    enabled: true
    interval: 30
  sampler:
    type: traceidratio
    ratio: 0.25
    parent: true
`

// TestReadSection_DecodesEveryField 是本文件最重要的用例。
//
// 它守的是「字段静默归零」这类故障：Config 的三个内嵌结构体曾经没有任何
// 序列化 tag，经 configx 读取时所有字段都解成零值，而 FillDefaults 随后又
// 把零值补成默认值 —— 于是配置写错和配置写对表现完全一样，只能靠直接断言
// 解码结果才能发现。删掉结构体上的 mapstructure tag，本用例必须失败。
func TestReadSection_DecodesEveryField(t *testing.T) {
	c, err := configx.ReadConfig(configx.ConfigOptions{ConfigFile: writeConfig(t, fullSection)})
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	cfg, err := readSection(c)
	if err != nil {
		t.Fatalf("readSection: %v", err)
	}

	if cfg.ServiceName != "myapp" {
		t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, "myapp")
	}
	if !cfg.Tracer.Enabled {
		t.Error("Tracer.Enabled = false, want true")
	}
	if cfg.Tracer.Exported != "none" {
		t.Errorf("Tracer.Exported = %q, want %q", cfg.Tracer.Exported, "none")
	}
	if cfg.Tracer.Endpoint != "collector.internal:4318" {
		t.Errorf("Tracer.Endpoint = %q, want %q", cfg.Tracer.Endpoint, "collector.internal:4318")
	}
	if !cfg.Meter.Enabled {
		t.Error("Meter.Enabled = false, want true")
	}
	if cfg.Meter.Interval != 30 {
		t.Errorf("Meter.Interval = %d, want 30", cfg.Meter.Interval)
	}
	if cfg.Sampler.Type != "traceidratio" {
		t.Errorf("Sampler.Type = %q, want %q", cfg.Sampler.Type, "traceidratio")
	}
	if cfg.Sampler.Ratio != 0.25 {
		t.Errorf("Sampler.Ratio = %v, want 0.25", cfg.Sampler.Ratio)
	}
	if !cfg.Sampler.Parent {
		t.Error("Sampler.Parent = false, want true")
	}
}

func TestReadAndCreate_ReturnsProvider(t *testing.T) {
	p, err := ReadAndCreate(configx.ConfigOptions{ConfigFile: writeConfig(t, fullSection)})
	if err != nil {
		t.Fatalf("ReadAndCreate: %v", err)
	}
	defer func() { _ = p.Close() }()

	if p.TracerProvider() == nil {
		t.Fatal("TracerProvider() = nil")
	}
	if p.MeterProvider() == nil {
		t.Fatal("MeterProvider() = nil")
	}
}

// otelx 段缺失时必须报错，而不是静默装配一个全默认的 Provider ——
// 否则调用方以为自己拿到了按配置装配的链路，实际是 noop/默认值。
func TestReadAndCreate_MissingSectionFails(t *testing.T) {
	p, err := ReadAndCreate(configx.ConfigOptions{
		ConfigFile: writeConfig(t, "logx:\n  level: info\n"),
	})
	if err == nil {
		if p != nil {
			_ = p.Close()
		}
		t.Fatal("ReadAndCreate with no otelx section must fail")
	}
	if !strings.Contains(err.Error(), "otelx") {
		t.Errorf("error %q should name the otelx section", err.Error())
	}
}

// 段存在但 tracer.enabled 为假：仍返回可用 Provider，只是 tracer 为 noop。
func TestReadAndCreate_DisabledSectionStillReturnsProvider(t *testing.T) {
	p, err := ReadAndCreate(configx.ConfigOptions{
		ConfigFile: writeConfig(t, "otelx:\n  service_name: svc\n  tracer:\n    enabled: false\n"),
	})
	if err != nil {
		t.Fatalf("ReadAndCreate: %v", err)
	}
	defer func() { _ = p.Close() }()

	if p.TracerProvider() == nil {
		t.Fatal("TracerProvider() = nil")
	}
}

func TestMustReadAndCreate_ReturnsProvider(t *testing.T) {
	if p := MustReadAndCreate(configx.ConfigOptions{ConfigFile: writeConfig(t, fullSection)}); p == nil {
		t.Fatal("MustReadAndCreate returned nil")
	}
}

// 无配置文件时 ReadAndCreate 应以"段不存在"失败，而不是 panic 或返回半成品。
func TestReadAndCreate_NoConfigFileFails(t *testing.T) {
	p, err := ReadAndCreate(configx.ConfigOptions{ConfigFile: filepath.Join(t.TempDir(), "absent.yaml")})
	if err == nil {
		if p != nil {
			_ = p.Close()
		}
		t.Fatal("ReadAndCreate without a config file must fail")
	}
}
