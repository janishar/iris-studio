package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"irisstudio/server"

	"github.com/fsnotify/fsnotify"
)

func main() {
	iris := flag.String("iris", "", "path to the iris binary (env IRISSTUDIO_IRIS; defaults to the last one used)")
	model := flag.String("model", "", "path to a downloaded model directory (env IRISSTUDIO_MODEL; defaults to the last one used)")
	port := flag.Int("port", 8720, "port")
	host := flag.String("host", "127.0.0.1", "host")
	dev := flag.Bool("dev", false, "enable hot reload (dev mode)")
	root := flag.String("root", "", "directory holding sessions/ (helmstudio passes its data directory here)")
	flag.Parse()

	// iris studio runs under helmstudio and nowhere else. Both of the things it
	// needs come from whatever launched it — the platform to record takes with,
	// and the directory to keep sessions in — and it makes up neither, so a
	// studio started by hand stops here instead of writing into a checkout.
	platform := server.NewPlatform()
	if !platform.Available() {
		if api := server.HelmAPI(); api != "" {
			fmt.Fprintf(os.Stderr, "iris studio runs under helmstudio, and %s could not be used — the reason is logged above.\n", api)
		} else {
			fmt.Fprintln(os.Stderr, "iris studio runs under helmstudio. HELM_API is not set, so there is no platform to run under.")
			fmt.Fprintln(os.Stderr, "  installed:  start it from helmstudio's Studios list")
			fmt.Fprintln(os.Stderr, "  a checkout: bash scripts/run.sh")
		}
		os.Exit(2)
	}
	rootDir := strings.TrimSpace(*root)
	if rootDir == "" {
		fmt.Fprintln(os.Stderr, "iris studio keeps its sessions in the directory helmstudio gives it and creates none of its own.")
		fmt.Fprintln(os.Stderr, "  --root is missing: helmstudio.yaml passes it as {data}.")
		os.Exit(2)
	}
	irisPath := firstNonEmpty(*iris, os.Getenv("IRISSTUDIO_IRIS"), server.SavedPath(rootDir, "iris.json", "iris"))
	modelPath := firstNonEmpty(*model, os.Getenv("IRISSTUDIO_MODEL"), server.SavedPath(rootDir, "model.json", "model"))
	if irisPath == "" || modelPath == "" {
		fmt.Fprintln(os.Stderr, "iris studio needs --iris and --model the first time it runs.")
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := server.NewConfig(server.Args{Iris: irisPath, Model: modelPath, Root: rootDir, Platform: platform})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if info, statErr := os.Stat(cfg.Iris); statErr != nil || info.IsDir() {
		fmt.Fprintf(os.Stderr, "iris binary not found: %s\n", cfg.Iris)
		os.Exit(1)
	}
	if !server.DirExists(cfg.Model) {
		fmt.Fprintf(os.Stderr, "model directory not found: %s\n", cfg.Model)
		os.Exit(1)
	}
	runner := server.NewRunner(cfg)
	app := server.NewApp(cfg, runner)
	srv := &http.Server{Addr: fmt.Sprintf("%s:%d", *host, *port), Handler: app}

	if *dev {
		watcher, err := startFileWatcher(cfg.Static, runner)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: file watcher failed to start: %v\n", err)
		} else {
			defer watcher.Close()
			fmt.Println("  File watcher: enabled (static files will auto-reload)")
		}
	} else {
		fmt.Println("  File watcher: disabled (use --dev to enable)")
	}

	fmt.Printf("iris studio  →  http://%s:%d\n", *host, *port)
	fmt.Printf("  binary   %s\n", cfg.Iris)
	fmt.Printf("  model    %s\n", cfg.Model)
	fmt.Printf("  sessions %s\n", cfg.Sessions)
	fmt.Printf("  helm     %s\n", server.HelmAPI())
	fmt.Printf("  inputs   %s\n", cfg.CurrentInputs())
	fmt.Printf("  outputs  %s\n", cfg.CurrentOutputs())

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		if sig != nil {
			fmt.Println("\nstopped")
		}
	case err := <-errCh:
		fmt.Fprintln(os.Stderr, err)
	}
	runner.StopInteractive()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// startFileWatcher watches the static directory for changes and triggers a reload.
func startFileWatcher(staticDir string, runner *server.Runner) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := watcher.Add(staticDir); err != nil {
		watcher.Close()
		return nil, err
	}
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write ||
					event.Op&fsnotify.Create == fsnotify.Create ||
					event.Op&fsnotify.Remove == fsnotify.Remove {
					if filepath.Ext(event.Name) != "" {
						fmt.Printf("  [Hot Reload] %s changed - reloading...\n", event.Name)
						runner.ReloadClients()
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				fmt.Fprintf(os.Stderr, "File watcher error: %v\n", err)
			}
		}
	}()
	return watcher, nil
}

// firstNonEmpty is the first of these that is set, trimmed, or "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
