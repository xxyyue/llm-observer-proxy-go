// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/xxyyue/llm-observer-proxy-go/internal/catalog"
	"github.com/xxyyue/llm-observer-proxy-go/internal/control"
	"github.com/xxyyue/llm-observer-proxy-go/internal/proxy"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "export" {
		if err := runExport(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "export:", err)
			os.Exit(1)
		}
		return
	}
	host := flag.String("host", "127.0.0.1", "control plane host")
	port := flag.Int("port", 8790, "control plane port")
	artifactRoot := flag.String("artifact-root", "", "artifact root (default: LLM_OBSERVER_PROXY_ARTIFACT_ROOT or ./artifacts)")
	modelCatalogPath := flag.String("model-catalog", "", "model catalog JSON (default: LLM_OBSERVER_PROXY_MODEL_CATALOG_PATH or ./config/models.json)")
	logLevel := flag.String("log-level", "INFO", "log level")
	envFile := flag.String("env-file", ".env", "optional .env file")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildVersion())
		return
	}
	if err := loadEnv(*envFile); err != nil {
		log.Fatalf("load env file: %v", err)
	}
	if *artifactRoot == "" {
		*artifactRoot = strings.TrimSpace(os.Getenv("LLM_OBSERVER_PROXY_ARTIFACT_ROOT"))
	}
	if *artifactRoot == "" {
		*artifactRoot = "artifacts"
	}
	if *modelCatalogPath == "" {
		*modelCatalogPath = strings.TrimSpace(os.Getenv("LLM_OBSERVER_PROXY_MODEL_CATALOG_PATH"))
	}
	modelCatalog, err := catalog.Load(*modelCatalogPath)
	if err != nil {
		log.Fatalf("load model catalog: %v", err)
	}
	rootContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine, err := proxy.NewNativeEngine(rootContext, modelCatalog, *logLevel)
	if err != nil {
		log.Fatalf("initialize embedded Bifrost: %v", err)
	}
	manager, err := proxy.NewManager(*artifactRoot, modelCatalog, engine)
	if err != nil {
		engine.Close()
		log.Fatalf("initialize proxy manager: %v", err)
	}
	controlServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", *host, *port),
		Handler:           control.NewServer(manager),
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-shutdown
		ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_ = controlServer.Shutdown(ctx)
	}()
	log.Printf("llm-observer-proxy %s listening on http://%s", version, controlServer.Addr)
	err = controlServer.ListenAndServe()
	manager.Close()
	if err != nil && err != http.ErrServerClosed {
		log.Fatalf("control plane failed: %v", err)
	}
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func loadEnv(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	file, err := os.Open(filepath.Clean(path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				_ = os.Setenv(key, value)
			}
		}
	}
	return scanner.Err()
}
