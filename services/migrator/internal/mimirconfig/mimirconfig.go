// Package mimirconfig provides a merge-safe helper for managing per-tenant
// overrides in the mimir-runtime ConfigMap. ApplyOverrides sets the three
// managed keys (out_of_order_time_window, ingestion_rate, ingestion_burst_size)
// for the given tenants while preserving all other existing data. RevertOverrides
// removes those keys (and the tenant entry if it becomes empty) without touching
// other tenants or per-tenant keys it does not own.
package mimirconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"gopkg.in/yaml.v3"
)

const (
	defaultNamespace     = "monitoring"
	defaultConfigMapName = "mimir-runtime"
	defaultDataKey       = "runtime.yaml"
	defaultOOOWindow     = "2880h"

	// The three keys this package owns. Only these are written/deleted;
	// all other per-tenant keys are left intact.
	keyOOO      = "out_of_order_time_window"
	keyIngRate  = "ingestion_rate"
	keyIngBurst = "ingestion_burst_size"

	ingestionRate      = 10_000_000
	ingestionBurstSize = 20_000_000
)

// Config holds the constructor arguments for Client.
type Config struct {
	K8s           kubernetes.Interface
	Namespace     string
	ConfigMapName string
	DataKey       string
	// OOOWindow is the out-of-order time window value written to every tenant,
	// e.g. "2880h". Defaults to "2880h" (120 days) when empty.
	OOOWindow string
	Logger    *slog.Logger
}

// Client applies and reverts Mimir per-tenant override entries in a ConfigMap.
type Client struct {
	k8s           kubernetes.Interface
	namespace     string
	configMapName string
	dataKey       string
	oooWindow     string
	logger        *slog.Logger
}

// NoopApplier is a no-op implementation of the api.OverrideApplier interface
// used during local development when no kubeconfig is available.
type NoopApplier struct{}

// ApplyOverrides is a no-op — it always succeeds without contacting Kubernetes.
func (NoopApplier) ApplyOverrides(_ context.Context, _ []string) error { return nil }

// New returns a Client with defaults applied for any zero-value Config fields.
func New(cfg Config) *Client {
	ns := cfg.Namespace
	if ns == "" {
		ns = defaultNamespace
	}
	cmName := cfg.ConfigMapName
	if cmName == "" {
		cmName = defaultConfigMapName
	}
	dk := cfg.DataKey
	if dk == "" {
		dk = defaultDataKey
	}
	ooo := cfg.OOOWindow
	if ooo == "" {
		ooo = defaultOOOWindow
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		k8s:           cfg.K8s,
		namespace:     ns,
		configMapName: cmName,
		dataKey:       dk,
		oooWindow:     ooo,
		logger:        logger,
	}
}

// ApplyOverrides sets the three managed keys for each tenant in the ConfigMap.
// Existing per-tenant keys that this package does not own are preserved, as are
// entries for tenants not in the provided list.
func (c *Client) ApplyOverrides(ctx context.Context, tenants []string) error {
	cm, err := c.k8s.CoreV1().ConfigMaps(c.namespace).Get(ctx, c.configMapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get configmap %s/%s: %w", c.namespace, c.configMapName, err)
	}

	doc, err := parseRuntimeYAML(cm, c.dataKey)
	if err != nil {
		return err
	}

	overrides := extractOverrides(doc)
	for _, tenant := range tenants {
		tm := overrides[tenant]
		if tm == nil {
			tm = make(map[string]interface{})
		}
		tm[keyOOO] = c.oooWindow
		tm[keyIngRate] = ingestionRate
		tm[keyIngBurst] = ingestionBurstSize
		overrides[tenant] = tm
	}
	doc["overrides"] = overrides

	if err := c.patchConfigMap(ctx, doc); err != nil {
		return err
	}

	c.logger.InfoContext(ctx, "applied mimir overrides", "tenants", tenants)
	return nil
}

// RevertOverrides removes the three managed keys for each tenant. If a tenant's
// map becomes empty after removal, the tenant entry itself is deleted. Tenants
// not found in the ConfigMap are silently skipped (idempotent).
func (c *Client) RevertOverrides(ctx context.Context, tenants []string) error {
	cm, err := c.k8s.CoreV1().ConfigMaps(c.namespace).Get(ctx, c.configMapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get configmap %s/%s: %w", c.namespace, c.configMapName, err)
	}

	doc, err := parseRuntimeYAML(cm, c.dataKey)
	if err != nil {
		return err
	}

	overrides := extractOverrides(doc)
	for _, tenant := range tenants {
		tm, ok := overrides[tenant]
		if !ok {
			continue
		}
		delete(tm, keyOOO)
		delete(tm, keyIngRate)
		delete(tm, keyIngBurst)
		if len(tm) == 0 {
			delete(overrides, tenant)
		} else {
			overrides[tenant] = tm
		}
	}
	if len(overrides) == 0 {
		delete(doc, "overrides")
	} else {
		doc["overrides"] = overrides
	}

	if err := c.patchConfigMap(ctx, doc); err != nil {
		return err
	}

	c.logger.InfoContext(ctx, "reverted mimir overrides", "tenants", tenants)
	return nil
}

// parseRuntimeYAML retrieves the YAML string from data[dataKey] and unmarshals
// it into a generic map. An absent or empty data key is treated as an empty
// document (no error).
func parseRuntimeYAML(cm *corev1.ConfigMap, dataKey string) (map[string]interface{}, error) {
	var yamlStr string
	if cm.Data != nil {
		yamlStr = cm.Data[dataKey]
	}
	doc := make(map[string]interface{})
	if yamlStr == "" {
		return doc, nil
	}
	if err := yaml.Unmarshal([]byte(yamlStr), &doc); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", dataKey, err)
	}
	return doc, nil
}

// extractOverrides returns the overrides sub-map from doc, converting yaml.v3's
// generic map[string]interface{} into map[string]map[string]interface{} so per-
// tenant entries can be mutated directly. If overrides is absent or malformed,
// an empty map is returned.
func extractOverrides(doc map[string]interface{}) map[string]map[string]interface{} {
	raw, ok := doc["overrides"]
	if !ok || raw == nil {
		return make(map[string]map[string]interface{})
	}
	switch v := raw.(type) {
	case map[string]map[string]interface{}:
		return v
	case map[string]interface{}:
		result := make(map[string]map[string]interface{}, len(v))
		for k, val := range v {
			switch tm := val.(type) {
			case map[string]interface{}:
				result[k] = tm
			default:
				result[k] = make(map[string]interface{})
			}
		}
		return result
	}
	return make(map[string]map[string]interface{})
}

// patchConfigMap marshals doc back to YAML and issues a MergePatch against the
// ConfigMap so only data[dataKey] is touched; other keys in data (if any) and
// all metadata are left intact.
func (c *Client) patchConfigMap(ctx context.Context, doc map[string]interface{}) error {
	yamlBytes, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal runtime yaml: %w", err)
	}

	patch := map[string]interface{}{
		"data": map[string]string{
			c.dataKey: string(yamlBytes),
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}

	_, err = c.k8s.CoreV1().ConfigMaps(c.namespace).Patch(
		ctx,
		c.configMapName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("patch configmap %s/%s: %w", c.namespace, c.configMapName, err)
	}
	return nil
}
