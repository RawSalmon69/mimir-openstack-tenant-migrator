package main

import (
	"testing"
)

func TestParseFlags_RequiresTenants(t *testing.T) {
	_, err := parseFlags([]string{})
	if err == nil {
		t.Fatal("expected error when -tenants is missing, got nil")
	}
}

func TestParseFlags_EmptyTenantsFlag(t *testing.T) {
	_, err := parseFlags([]string{"-tenants", "  "})
	if err == nil {
		t.Fatal("expected error for blank -tenants value")
	}
}

func TestParseFlags_SingleTenant(t *testing.T) {
	cfg, err := parseFlags([]string{"-tenants", "acme"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.tenants) != 1 || cfg.tenants[0] != "acme" {
		t.Errorf("expected [acme], got %v", cfg.tenants)
	}
}

func TestParseFlags_CommaSplit(t *testing.T) {
	cfg, err := parseFlags([]string{"-tenants", "acme,widgets, beta "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"acme", "widgets", "beta"}
	if len(cfg.tenants) != len(want) {
		t.Fatalf("expected %v, got %v", want, cfg.tenants)
	}
	for i, v := range want {
		if cfg.tenants[i] != v {
			t.Errorf("tenants[%d]: want %q, got %q", i, v, cfg.tenants[i])
		}
	}
}

func TestParseFlags_Defaults(t *testing.T) {
	cfg, err := parseFlags([]string{"-tenants", "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.namespace != "monitoring" {
		t.Errorf("default namespace: want monitoring, got %q", cfg.namespace)
	}
	if cfg.configmap != "mimir-runtime" {
		t.Errorf("default configmap: want mimir-runtime, got %q", cfg.configmap)
	}
	if cfg.dataKey != "runtime.yaml" {
		t.Errorf("default data-key: want runtime.yaml, got %q", cfg.dataKey)
	}
}

func TestParseFlags_CustomFlags(t *testing.T) {
	cfg, err := parseFlags([]string{
		"-tenants", "t1",
		"-namespace", "prod",
		"-configmap", "my-cm",
		"-data-key", "config.yaml",
		"-kubeconfig", "/tmp/kube",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.namespace != "prod" {
		t.Errorf("namespace: want prod, got %q", cfg.namespace)
	}
	if cfg.configmap != "my-cm" {
		t.Errorf("configmap: want my-cm, got %q", cfg.configmap)
	}
	if cfg.dataKey != "config.yaml" {
		t.Errorf("data-key: want config.yaml, got %q", cfg.dataKey)
	}
	if cfg.kubeconfig != "/tmp/kube" {
		t.Errorf("kubeconfig: want /tmp/kube, got %q", cfg.kubeconfig)
	}
}

func TestSplitTenants(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{"a, b , c", []string{"a", "b", "c"}},
		{"single", []string{"single"}},
		{"a,,b", []string{"a", "b"}}, // empty segment dropped
		{"  ", nil},
	}
	for _, tc := range cases {
		got := splitTenants(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("splitTenants(%q): want %v, got %v", tc.input, tc.want, got)
			continue
		}
		for i, v := range tc.want {
			if got[i] != v {
				t.Errorf("splitTenants(%q)[%d]: want %q, got %q", tc.input, i, v, got[i])
			}
		}
	}
}
