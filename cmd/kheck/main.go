// kheck is an MCP server in front of the Kubernetes API: the only way agents
// change the cluster. Each change passes its gates, is recorded in tremolo as
// the agent that asked, and is waited on.
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

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tpural/kheck/internal/config"
	"github.com/tpural/kheck/internal/gate"
	"github.com/tpural/kheck/internal/kube"
	"github.com/tpural/kheck/internal/server"
	"github.com/tpural/kheck/internal/tremolo"
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

func run(path string, log *slog.Logger) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return err
	}
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
	t := tremolo.New(cfg.Tremolo.URL)
	s := &server.Server{
		Config: cfg, Cluster: cluster, Tremolo: t, Log: log, Version: version,
		Gates: &gate.Gates{Gates: cfg.Gates, Cluster: cluster, Metrics: t, AsleepTaints: cfg.AsleepTaints},
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), cfg.Settle.Duration+10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Info("kheck", "version", version, "listen", cfg.Listen, "gates", len(cfg.Gates), "protected", cfg.Protect.Namespaces)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
