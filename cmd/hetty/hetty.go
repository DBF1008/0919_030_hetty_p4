package main

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/gorilla/mux"
	"github.com/mitchellh/go-homedir"
	"github.com/peterbourgon/ff/v3/ffcli"
	"go.uber.org/zap"

	"github.com/dstotijn/hetty/pkg/api"
	"github.com/dstotijn/hetty/pkg/chrome"
	"github.com/dstotijn/hetty/pkg/db/bolt"
	"github.com/dstotijn/hetty/pkg/proj"
	"github.com/dstotijn/hetty/pkg/proxy"
	"github.com/dstotijn/hetty/pkg/proxy/intercept"
	"github.com/dstotijn/hetty/pkg/reqlog"
	"github.com/dstotijn/hetty/pkg/scope"
	"github.com/dstotijn/hetty/pkg/sender"
)

var version = "0.0.0"

//go:embed admin
//go:embed admin/_next/static
//go:embed admin/_next/static/chunks/pages/*.js
//go:embed admin/_next/static/*/*.js
var adminContent embed.FS

var hettyUsage = `
Usage:
    hetty [flags] [subcommand] [flags]

Runs an HTTP server with (MITM) proxy, GraphQL service, and a web based admin interface.

Options:
    --cert         Path to root CA certificate. Creates file if it doesn't exist. (Default: "~/.hetty/hetty_cert.pem")
    --key          Path to root CA private key. Creates file if it doesn't exist. (Default: "~/.hetty/hetty_key.pem")
    --db           Database file path. Creates file if it doesn't exist. (Default: "~/.hetty/hetty.db")
    --addr         TCP address for HTTP server to listen on, in the form \"host:port\". (Default: ":8080")
    --chrome       Launch Chrome with proxy settings applied and certificate errors ignored. (Default: false)
    --verbose      Enable verbose logging.
    --json         Encode logs as JSON, instead of pretty/human readable output.
    --version, -v  Output version.
    --help, -h     Output this usage text.

Subcommands:
    - cert  Certificate management

Run ` + "`hetty <subcommand> --help`" + ` for subcommand specific usage instructions.

Visit https://hetty.xyz to learn more about Hetty.
`

type HettyCommand struct {
	config *Config

	cert    string
	key     string
	db      string
	addr    string
	chrome  bool
	version bool
}

func NewHettyCommand() (*ffcli.Command, *Config) {
	cmd := HettyCommand{
		config: &Config{},
	}

	fs := flag.NewFlagSet("hetty", flag.ExitOnError)

	fs.StringVar(&cmd.cert, "cert", "~/.hetty/hetty_cert.pem",
		"Path to root CA certificate. Creates a new certificate if file doesn't exist.")
	fs.StringVar(&cmd.key, "key", "~/.hetty/hetty_key.pem",
		"Path to root CA private key. Creates a new private key if file doesn't exist.")
	fs.StringVar(&cmd.db, "db", "~/.hetty/hetty.db", "Database file path. Creates file if it doesn't exist.")
	fs.StringVar(&cmd.addr, "addr", ":8080", "TCP address to listen on, in the form \"host:port\".")
	fs.BoolVar(&cmd.chrome, "chrome", false, "Launch Chrome with proxy settings applied and certificate errors ignored.")
	fs.BoolVar(&cmd.version, "version", false, "Output version.")
	fs.BoolVar(&cmd.version, "v", false, "Output version.")

	cmd.config.RegisterFlags(fs)

	return &ffcli.Command{
		Name:    "hetty",
		FlagSet: fs,
		Subcommands: []*ffcli.Command{
			NewCertCommand(cmd.config),
		},
		Exec: cmd.Exec,
		UsageFunc: func(*ffcli.Command) string {
			return hettyUsage
		},
	}, cmd.config
}

func (cmd *HettyCommand) Exec(ctx context.Context, _ []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	if cmd.version {
		fmt.Fprint(os.Stdout, version+"\n")
		return nil
	}

	mainLogger := cmd.config.logger.Named("main")

	listenHost, listenPort, err := net.SplitHostPort(cmd.addr)
	if err != nil {
		return fmt.Errorf("failed to parse listening address: %w", err)
	}

	url := fmt.Sprintf("http://%v:%v", listenHost, listenPort)
	if listenHost == "" || listenHost == "0.0.0.0" || listenHost == "127.0.0.1" || listenHost == "::1" {
		url = fmt.Sprintf("http://localhost:%v", listenPort)
	}

	// Expand `~` in filepaths.
	caCertFile, err := homedir.Expand(cmd.cert)
	if err != nil {
		return fmt.Errorf("failed to parse CA certificate filepath: %w", err)
	}

	caKeyFile, err := homedir.Expand(cmd.key)
	if err != nil {
		return fmt.Errorf("failed to parse CA private key filepath: %w", err)
	}

	dbPath, err := homedir.Expand(cmd.db)
	if err != nil {
		return fmt.Errorf("failed to parse database path: %w", err)
	}

	// Load existing CA certificate and key from disk, or generate and write
	// to disk if no files exist yet.
	caCert, caKey, err := proxy.LoadOrCreateCA(caKeyFile, caCertFile)
	if err != nil {
		return fmt.Errorf("failed to load or create CA key pair: %w", err)
	}

	dbLogger := cmd.config.logger.Named("boltdb").Sugar()
	boltOpts := bolt.DefaultOptions()
	boltOpts.Logger = &bolt.Logger{SugaredLogger: dbLogger}

	boltDB, err := bolt.OpenDatabase(dbPath, boltOpts)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer func() {
		if err := boltDB.Close(); err != nil {
			mainLogger.Error("Failed to close database.", zap.Error(err))
		}
	}()

	scope := &scope.Scope{}

	reqLogService := reqlog.NewService(reqlog.Config{
		Scope:      scope,
		Repository: boltDB,
		Logger:     cmd.config.logger.Named("reqlog").Sugar(),
	})

	interceptService := intercept.NewService(intercept.Config{
		Logger: cmd.config.logger.Named("intercept").Sugar(),
	})

	senderService := sender.NewService(sender.Config{
		Repository:    boltDB,
		ReqLogService: reqLogService,
	})

	projService, err := proj.NewService(proj.Config{
		Repository:       boltDB,
		InterceptService: interceptService,
		ReqLogService:    reqLogService,
		SenderService:    senderService,
		Scope:            scope,
	})
	if err != nil {
		return fmt.Errorf("failed to create new projects service: %w", err)
	}

	proxy, err := proxy.NewProxy(proxy.Config{
		CACert: caCert,
		CAKey:  caKey,
		Logger: cmd.config.logger.Named("proxy").Sugar(),
	})
	if err != nil {
		return fmt.Errorf("failed to create new proxy: %w", err)
	}

	proxy.UseRequestModifier(reqLogService.RequestModifier)
	proxy.UseResponseModifier(reqLogService.ResponseModifier)
	proxy.UseRequestModifier(interceptService.RequestModifier)
	proxy.UseResponseModifier(interceptService.ResponseModifier)

	fsSub, err := fs.Sub(adminContent, "admin")
	if err != nil {
		return fmt.Errorf("failed to construct file system subtree from admin dir: %w", err)
	}

	adminHandler := http.FileServer(http.FS(fsSub))
	router := mux.NewRouter().SkipClean(true)
	adminRouter := router.MatcherFunc(func(req *http.Request, match *mux.RouteMatch) bool {
		hostname, _ := os.Hostname()
		host, _, _ := net.SplitHostPort(req.Host)

		// Serve local admin routes when either:
		// - The `Host` is well-known, e.g. `hetty.proxy`, `localhost:[port]`
		//   or the listen addr `[host]:[port]`.
		// - The request is not for TLS proxying (e.g. no `CONNECT`) and not
		//   for proxying an external URL. E.g. Request-Line (RFC 7230, Section 3.1.1)
		//   has no scheme.
		return strings.EqualFold(host, hostname) ||
			req.Host == "hetty.proxy" ||
			req.Host == fmt.Sprintf("%v:%v", "localhost", listenPort) ||
			req.Host == fmt.Sprintf("%v:%v", listenHost, listenPort) ||
			req.Method != http.MethodConnect && !strings.HasPrefix(req.RequestURI, "http://")
	}).Subrouter().StrictSlash(true)

	// GraphQL server.
	gqlEndpoint := "/api/graphql/"
	adminRouter.Path(gqlEndpoint).Handler(api.HTTPHandler(&api.Resolver{
		ProjectService:    projService,
		RequestLogService: reqLogService,
		InterceptService:  interceptService,
		SenderService:     senderService,
	}, gqlEndpoint))

	// Admin interface.
	adminRouter.PathPrefix("").Handler(adminHandler)

	// Fallback (default) is the Proxy handler.
	router.PathPrefix("").Handler(proxy)

	httpServer := &http.Server{
		Addr:         cmd.addr,
		Handler:      router,
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}, // Disable HTTP/2
		ErrorLog:     zap.NewStdLog(cmd.config.logger.Named("http")),
	}

	// serverErrCh receives the result of httpServer.ListenAndServe. A nil
	// value (or http.ErrServerClosed) means the server was closed gracefully.
	serverErrCh := make(chan error, 1)

	go func() {
		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}

		serverErrCh <- err
	}()

	// Startup health check: poll the admin interface until the HTTP server
	// accepts requests, or fail if the server exits (e.g. address in use).
	healthCh := make(chan error, 1)

	go func() {
		healthCh <- waitForHealthy(url, 5*time.Second)
	}()

	select {
	case err := <-serverErrCh:
		if err != nil {
			return fmt.Errorf("HTTP server closed unexpectedly: %w", err)
		}

		return nil
	case err := <-healthCh:
		if err != nil {
			// The server didn't become healthy in time; close it before
			// returning so no goroutine is left running.
			httpServer.Close()

			return fmt.Errorf("startup health check failed: %w", err)
		}
	}

	mainLogger.Info("Startup health check passed.")
	mainLogger.Info(fmt.Sprintf("Hetty (v%v) is running on %v ...", version, cmd.addr))
	mainLogger.Info(fmt.Sprintf("\x1b[%dm%s\x1b[0m", uint8(32), "Get started at "+url))

	if cmd.chrome {
		ctx, cancel := chrome.NewExecAllocator(ctx, chrome.Config{
			ProxyServer:      url,
			ProxyBypassHosts: []string{url},
		})
		defer cancel()

		taskCtx, cancel := chromedp.NewContext(ctx)
		defer cancel()

		err = chromedp.Run(taskCtx, chromedp.Navigate(url))

		switch {
		case errors.Is(err, exec.ErrNotFound):
			mainLogger.Info("Chrome executable not found.")
		case err != nil:
			mainLogger.Error(fmt.Sprintf("Failed to navigate to %v.", url), zap.Error(err))
		default:
			mainLogger.Info("Launched Chrome.")
		}
	}

	// Wait for interrupt signal or unexpected server error.
	select {
	case <-ctx.Done():
		// Restore signal, allowing "force quit".
		stop()
	case err := <-serverErrCh:
		if err != nil {
			return fmt.Errorf("HTTP server closed unexpectedly: %w", err)
		}

		return nil
	}

	mainLogger.Info("Shutting down HTTP server. Press Ctrl+C to force quit.")

	// Close the proxy first: this forcefully closes hijacked CONNECT tunnel
	// connections (which http.Server.Shutdown does not track, so clients
	// waiting on a response get notified) and idle keep-alive connections
	// in the proxy's transport.
	proxy.Close()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = httpServer.Shutdown(shutdownCtx)
	if err != nil {
		return fmt.Errorf("failed to shutdown HTTP server: %w", err)
	}

	return nil
}

// waitForHealthy polls the given URL until it returns an HTTP response or
// the timeout elapses.
func waitForHealthy(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)

	for {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()

			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("no response within %v: %w", timeout, err)
		}

		time.Sleep(100 * time.Millisecond)
	}
}
