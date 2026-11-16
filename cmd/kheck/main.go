// kheck is an MCP server in front of the Kubernetes API: the only way agents
// change the cluster. Each change passes its gates, is made as kheck, and is
// logged with the caller who asked, to stderr and over OpenTelemetry.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tpural/kheck/internal/config"
	"github.com/tpural/kheck/internal/gate"
	"github.com/tpural/kheck/internal/kube"
	"github.com/tpural/kheck/internal/metrics"
	"github.com/tpural/kheck/internal/server"
)

var version = "dev"

func main() {
	path := flag.String("config", "/etc/kheck/config.yaml", "config file")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(*path, log); err != nil {
		log.Error("kheck", "err", err)
		os.Exit(1)
	}
}

// otlp sends logs over OTLP/HTTP when OTEL_EXPORTER_OTLP_ENDPOINT (or
// OTEL_EXPORTER_OTLP_LOGS_ENDPOINT) is set; the exporter reads the rest of
// the standard OTEL_* variables, e.g. headers.
func otlp(ctx context.Context, log *slog.Logger) (*slog.Logger, func(), error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") == "" {
		return log, func() {}, nil
	}
	exp, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "kheck"), attribute.String("service.version", version)),
		resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil {
		return nil, nil, err
	}
	p := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)))
	h := slog.NewMultiHandler(log.Handler(), otelslog.NewHandler("kheck", otelslog.WithLoggerProvider(p), otelslog.WithVersion(version)))
	return slog.New(h), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	}, nil
}

func run(path string, log *slog.Logger) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log, flush, err := otlp(ctx, log)
	if err != nil {
		return err
	}
	defer flush()
	rc, err := rest.InClusterConfig()
	if errors.Is(err, rest.ErrNotInCluster) {
		rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	}
	if err != nil {
		return err
	}
	k, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	cluster := kube.New(k)
	g := &gate.Gates{Gates: cfg.Gates, Cluster: cluster, AsleepTaints: cfg.AsleepTaints}
	if cfg.Metrics.URL != "" {
		g.Metrics = metrics.New(cfg.Metrics.URL)
	}
	s := &server.Server{Config: cfg, Cluster: cluster, Gates: g, Log: log, Version: version}
	srv := &http.Server{Addr: cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), cfg.Settle.Duration+10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Info("kheck", "version", version, "listen", cfg.Listen, "callers", cfg.Callers, "gates", len(cfg.Gates), "protected", cfg.Protect.Namespaces)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
