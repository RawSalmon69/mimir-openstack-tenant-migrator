package mimirconfig

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"gopkg.in/yaml.v3"
)

const (
	testNS    = "monitoring"
	testCM    = "mimir-runtime"
	testKey   = "runtime.yaml"
	tenantA   = "tenant-A"
	tenantB   = "tenant-B"
	tenantC   = "tenant-C"
)

// buildCM creates a ConfigMap with the given runtime.yaml content.
func buildCM(runtimeYAML string) *corev1.ConfigMap {
	data := map[string]string{}
	if runtimeYAML != "" {
		data[testKey] = runtimeYAML
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testCM,
			Namespace: testNS,
		},
		Data: data,
	}
}

// newClient returns a Client wired to the provided fake clientset using
// test-scoped defaults (namespace, CM name, data key, default OOO window).
func newClient(cs *fake.Clientset) *Client {
	return New(Config{
		K8s:           cs,
		Namespace:     testNS,
		ConfigMapName: testCM,
		DataKey:       testKey,
	})
}

// readOverrides reads the runtime.yaml from the fake API server and returns the
// parsed overrides map, or an empty map if the overrides key is absent.
func readOverrides(t *testing.T, cs *fake.Clientset) map[string]map[string]interface{} {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(testNS).Get(context.Background(), testCM, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read configmap: %v", err)
	}
	doc := make(map[string]interface{})
	if yamlStr := cm.Data[testKey]; yamlStr != "" {
		if err := yaml.Unmarshal([]byte(yamlStr), &doc); err != nil {
			t.Fatalf("unmarshal runtime.yaml: %v", err)
		}
	}
	return extractOverrides(doc)
}

// assertManagedKeys checks that the three managed keys for the given tenant are
// present with the correct values.
func assertManagedKeys(t *testing.T, overrides map[string]map[string]interface{}, tenant, expectedOOO string) {
	t.Helper()
	tm, ok := overrides[tenant]
	if !ok {
		t.Fatalf("tenant %q not found in overrides", tenant)
	}

	if got := fmt.Sprintf("%v", tm[keyOOO]); got != expectedOOO {
		t.Errorf("tenant %s: %s = %q, want %q", tenant, keyOOO, got, expectedOOO)
	}
	if got, _ := toInt(tm[keyIngRate]); got != ingestionRate {
		t.Errorf("tenant %s: %s = %v, want %d", tenant, keyIngRate, tm[keyIngRate], ingestionRate)
	}
	if got, _ := toInt(tm[keyIngBurst]); got != ingestionBurstSize {
		t.Errorf("tenant %s: %s = %v, want %d", tenant, keyIngBurst, tm[keyIngBurst], ingestionBurstSize)
	}
}

// toInt converts yaml numeric types to int so comparisons work regardless of
// whether yaml.v3 decoded the number as int, int64, float64, etc.
func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// TestApplyMergesWithExistingOverrides verifies that applying overrides for
// tenant-A and tenant-B does not disturb a pre-existing tenant-C entry.
func TestApplyMergesWithExistingOverrides(t *testing.T) {
	seedYAML := `overrides:
  tenant-C:
    out_of_order_time_window: 2160h
`
	cs := fake.NewSimpleClientset(buildCM(seedYAML))
	c := newClient(cs)

	if err := c.ApplyOverrides(context.Background(), []string{tenantA, tenantB}); err != nil {
		t.Fatalf("ApplyOverrides: %v", err)
	}

	overrides := readOverrides(t, cs)

	assertManagedKeys(t, overrides, tenantA, defaultOOOWindow)
	assertManagedKeys(t, overrides, tenantB, defaultOOOWindow)

	// tenant-C must be untouched.
	tc, ok := overrides[tenantC]
	if !ok {
		t.Fatalf("tenant-C entry missing after merge")
	}
	if got := fmt.Sprintf("%v", tc[keyOOO]); got != "2160h" {
		t.Errorf("tenant-C out_of_order_time_window = %q, want 2160h", got)
	}
}

// TestApplyPreservesUnknownPerTenantKeys verifies that a custom key present on
// a tenant's map survives an apply operation.
func TestApplyPreservesUnknownPerTenantKeys(t *testing.T) {
	seedYAML := `overrides:
  tenant-A:
    max_global_series_per_user: 500000
`
	cs := fake.NewSimpleClientset(buildCM(seedYAML))
	c := newClient(cs)

	if err := c.ApplyOverrides(context.Background(), []string{tenantA}); err != nil {
		t.Fatalf("ApplyOverrides: %v", err)
	}

	overrides := readOverrides(t, cs)
	assertManagedKeys(t, overrides, tenantA, defaultOOOWindow)

	tm := overrides[tenantA]
	val, ok := tm["max_global_series_per_user"]
	if !ok {
		t.Fatalf("custom key max_global_series_per_user was removed")
	}
	n, _ := toInt(val)
	if n != 500000 {
		t.Errorf("max_global_series_per_user = %v, want 500000", val)
	}
}

// TestApplyIsIdempotent verifies that calling ApplyOverrides twice for the same
// tenant produces exactly one correct entry (no duplicates, no drift).
func TestApplyIsIdempotent(t *testing.T) {
	cs := fake.NewSimpleClientset(buildCM(""))
	c := newClient(cs)

	for i := 0; i < 2; i++ {
		if err := c.ApplyOverrides(context.Background(), []string{tenantA}); err != nil {
			t.Fatalf("ApplyOverrides call %d: %v", i+1, err)
		}
	}

	overrides := readOverrides(t, cs)
	if len(overrides) != 1 {
		t.Errorf("expected 1 tenant in overrides, got %d", len(overrides))
	}
	assertManagedKeys(t, overrides, tenantA, defaultOOOWindow)
}

// TestApplyHandlesMissingRuntimeYaml verifies that a ConfigMap with an empty
// data map gets the overrides block created correctly.
func TestApplyHandlesMissingRuntimeYaml(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCM, Namespace: testNS},
		Data:       map[string]string{}, // explicitly empty
	}
	cs := fake.NewSimpleClientset(cm)
	c := newClient(cs)

	if err := c.ApplyOverrides(context.Background(), []string{tenantA}); err != nil {
		t.Fatalf("ApplyOverrides: %v", err)
	}

	overrides := readOverrides(t, cs)
	assertManagedKeys(t, overrides, tenantA, defaultOOOWindow)
}

// TestApplyUses2880hOOOWindow verifies that the default OOO window is "2880h".
func TestApplyUses2880hOOOWindow(t *testing.T) {
	cs := fake.NewSimpleClientset(buildCM(""))
	c := newClient(cs) // uses defaultOOOWindow

	if err := c.ApplyOverrides(context.Background(), []string{tenantA}); err != nil {
		t.Fatalf("ApplyOverrides: %v", err)
	}

	overrides := readOverrides(t, cs)
	tm, ok := overrides[tenantA]
	if !ok {
		t.Fatalf("tenant-A not in overrides")
	}
	if got := fmt.Sprintf("%v", tm[keyOOO]); got != "2880h" {
		t.Errorf("out_of_order_time_window = %q, want 2880h", got)
	}
}

// TestRevertRemovesOnlyManagedKeys verifies that Revert deletes the three
// managed keys but leaves unmanaged keys on the tenant's map intact.
func TestRevertRemovesOnlyManagedKeys(t *testing.T) {
	seedYAML := `overrides:
  tenant-A:
    out_of_order_time_window: 2880h
    ingestion_rate: 1000000
    ingestion_burst_size: 2000000
    max_global_series_per_user: 500000
`
	cs := fake.NewSimpleClientset(buildCM(seedYAML))
	c := newClient(cs)

	if err := c.RevertOverrides(context.Background(), []string{tenantA}); err != nil {
		t.Fatalf("RevertOverrides: %v", err)
	}

	overrides := readOverrides(t, cs)
	tm, ok := overrides[tenantA]
	if !ok {
		t.Fatalf("tenant-A entry deleted; expected it to survive because of custom key")
	}
	if _, exists := tm[keyOOO]; exists {
		t.Errorf("managed key %q still present after revert", keyOOO)
	}
	if _, exists := tm[keyIngRate]; exists {
		t.Errorf("managed key %q still present after revert", keyIngRate)
	}
	if _, exists := tm[keyIngBurst]; exists {
		t.Errorf("managed key %q still present after revert", keyIngBurst)
	}
	val, ok2 := tm["max_global_series_per_user"]
	if !ok2 {
		t.Fatalf("custom key max_global_series_per_user was removed during revert")
	}
	n, _ := toInt(val)
	if n != 500000 {
		t.Errorf("max_global_series_per_user = %v, want 500000", val)
	}
}

// TestRevertDeletesEmptyTenantEntry verifies that a tenant entry is fully removed
// from the overrides map when its last keys are the three managed ones.
func TestRevertDeletesEmptyTenantEntry(t *testing.T) {
	seedYAML := `overrides:
  tenant-A:
    out_of_order_time_window: 2880h
    ingestion_rate: 1000000
    ingestion_burst_size: 2000000
`
	cs := fake.NewSimpleClientset(buildCM(seedYAML))
	c := newClient(cs)

	if err := c.RevertOverrides(context.Background(), []string{tenantA}); err != nil {
		t.Fatalf("RevertOverrides: %v", err)
	}

	overrides := readOverrides(t, cs)
	if _, ok := overrides[tenantA]; ok {
		t.Errorf("tenant-A entry should have been deleted but is still present: %v", overrides[tenantA])
	}
}

// TestRevertIsIdempotent verifies that reverting a tenant that was never applied
// (or already reverted) returns no error and leaves the ConfigMap stable.
func TestRevertIsIdempotent(t *testing.T) {
	cs := fake.NewSimpleClientset(buildCM(""))
	c := newClient(cs)

	// First revert on an empty CM — should be a no-op.
	if err := c.RevertOverrides(context.Background(), []string{tenantA}); err != nil {
		t.Fatalf("first RevertOverrides: %v", err)
	}

	// Second revert — same expectation.
	if err := c.RevertOverrides(context.Background(), []string{tenantA}); err != nil {
		t.Fatalf("second RevertOverrides: %v", err)
	}

	overrides := readOverrides(t, cs)
	if len(overrides) != 0 {
		t.Errorf("expected empty overrides, got: %v", overrides)
	}
}
