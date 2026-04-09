// Command revert-overrides removes the Mimir OOO + rate-limit overrides for
// one or more tenants from the mimir-runtime ConfigMap.
//
// Usage:
//
//	revert-overrides -tenants tenantA,tenantB [-namespace monitoring] \
//	    [-configmap mimir-runtime] [-data-key runtime.yaml] [-kubeconfig ~/.kube/config]
//
// Example (in-cluster):
//
//	revert-overrides -tenants acme,widgets
//
// Example (local with explicit kubeconfig):
//
//	revert-overrides -tenants acme,widgets -kubeconfig ~/.kube/config -namespace monitoring
//
// The binary tries in-cluster auth first. If that fails it falls back to the
// value supplied with -kubeconfig (or $KUBECONFIG). If neither works the
// command exits 1 with a descriptive error.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/NIPA-Mimir/services/migrator/internal/mimirconfig"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\nRun with -h for usage.\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg); err != nil {
		slog.Error("revert-overrides failed", "error", err)
		os.Exit(1)
	}
}

// cliConfig holds the parsed flag values.
type cliConfig struct {
	tenants    []string
	namespace  string
	configmap  string
	dataKey    string
	kubeconfig string
}

// parseFlags parses args and returns a cliConfig or an error. Separating this
// from main() lets tests exercise flag validation without exec'ing a subprocess.
func parseFlags(args []string) (*cliConfig, error) {
	fs := flag.NewFlagSet("revert-overrides", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `revert-overrides — remove Mimir OOO + rate-limit overrides for tenants

Usage:
  revert-overrides -tenants tenantA,tenantB [flags]

Example:
  revert-overrides -tenants acme,widgets -namespace monitoring -kubeconfig ~/.kube/config

Flags:
`)
		fs.PrintDefaults()
	}

	var tenantsRaw string
	fs.StringVar(&tenantsRaw, "tenants", "", "Comma-separated list of tenant IDs to revert (required)")
	namespace := fs.String("namespace", "monitoring", "Kubernetes namespace of the ConfigMap")
	configmap := fs.String("configmap", "mimir-runtime", "Name of the mimir-runtime ConfigMap")
	dataKey := fs.String("data-key", "runtime.yaml", "Key inside ConfigMap.data that holds the YAML")
	kubeconfig := fs.String("kubeconfig", os.Getenv("KUBECONFIG"), "Path to kubeconfig file (defaults to $KUBECONFIG; in-cluster auth tried first)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if strings.TrimSpace(tenantsRaw) == "" {
		return nil, fmt.Errorf("-tenants is required and must not be empty")
	}

	tenants := splitTenants(tenantsRaw)
	if len(tenants) == 0 {
		return nil, fmt.Errorf("-tenants produced an empty list after splitting %q", tenantsRaw)
	}

	return &cliConfig{
		tenants:    tenants,
		namespace:  *namespace,
		configmap:  *configmap,
		dataKey:    *dataKey,
		kubeconfig: *kubeconfig,
	}, nil
}

// splitTenants splits a comma-separated string and trims whitespace, discarding
// empty segments.
func splitTenants(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// buildK8sClient tries in-cluster config first, then falls back to the
// kubeconfig file path supplied. Returns an error if neither works.
func buildK8sClient(kubeconfigPath string) (kubernetes.Interface, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		restCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("could not build kubernetes client config (tried in-cluster and kubeconfig %q): %w", kubeconfigPath, err)
		}
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	return client, nil
}

// run is the core logic so it can be unit-tested without exec.
func run(ctx context.Context, cfg *cliConfig) error {
	k8sClient, err := buildK8sClient(cfg.kubeconfig)
	if err != nil {
		return err
	}

	mc := mimirconfig.New(mimirconfig.Config{
		K8s:           k8sClient,
		Namespace:     cfg.namespace,
		ConfigMapName: cfg.configmap,
		DataKey:       cfg.dataKey,
		OOOWindow:     "2880h",
	})

	start := time.Now()
	slog.Info("reverting mimir overrides", "tenants", cfg.tenants, "namespace", cfg.namespace, "configmap", cfg.configmap)

	if err := mc.RevertOverrides(ctx, cfg.tenants); err != nil {
		return fmt.Errorf("RevertOverrides: %w", err)
	}

	slog.Info("overrides reverted successfully", "tenants", cfg.tenants, "elapsed", time.Since(start).Round(time.Millisecond))
	return nil
}
