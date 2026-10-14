// Command external-dns-transip-webhook is an ExternalDNS webhook provider for
// TransIP. It runs as a sidecar of ExternalDNS, which talks to it on localhost.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/provider/webhook/api"

	"github.com/lion7/external-dns-transip-webhook/internal/transip"
)

type config struct {
	account       string
	keyFile       string
	timeout       time.Duration
	domainFilter  []string
	dryRun        bool
	listenAddress string
	healthAddress string
	logLevel      string
	logFormat     string
}

// parseConfig reads the configuration from the flags, every flag defaults to
// its environment variable.
func parseConfig(args []string, getenv func(string) string) (*config, error) {
	env := func(name, fallback string) string {
		if value := getenv(name); value != "" {
			return value
		}
		return fallback
	}

	dryRun, err := strconv.ParseBool(env("DRY_RUN", "false"))
	if err != nil {
		return nil, fmt.Errorf("invalid DRY_RUN: %w", err)
	}

	timeout, err := time.ParseDuration(env("TRANSIP_TIMEOUT", "5s"))
	if err != nil {
		return nil, fmt.Errorf("invalid TRANSIP_TIMEOUT: %w", err)
	}

	cfg := &config{}
	fs := flag.NewFlagSet("external-dns-transip-webhook", flag.ContinueOnError)
	fs.StringVar(&cfg.account, "transip-account", env("TRANSIP_ACCOUNT", ""),
		"TransIP account name (env TRANSIP_ACCOUNT)")
	fs.StringVar(&cfg.keyFile, "transip-keyfile", env("TRANSIP_KEYFILE", ""),
		"path to the private key of the TransIP account (env TRANSIP_KEYFILE)")
	fs.DurationVar(&cfg.timeout, "transip-timeout", timeout,
		"give up on a TransIP API call after this long; keep it well below the 15s ExternalDNS waits for the webhook (env TRANSIP_TIMEOUT)")
	fs.Func("domain-filter",
		"limit the zones to this domain, can be repeated (env DOMAIN_FILTER, comma separated); all zones of the account when not set",
		func(value string) error {
			cfg.domainFilter = append(cfg.domainFilter, value)
			return nil
		})
	fs.BoolVar(&cfg.dryRun, "dry-run", dryRun,
		"log the changes instead of applying them; --dry-run of ExternalDNS does not reach a webhook provider (env DRY_RUN)")
	fs.StringVar(&cfg.listenAddress, "listen-address", env("LISTEN_ADDRESS", "127.0.0.1:8888"),
		"address of the webhook API, which has no authentication (env LISTEN_ADDRESS)")
	fs.StringVar(&cfg.healthAddress, "health-address", env("HEALTH_ADDRESS", ":8080"),
		"address of the /healthz endpoint (env HEALTH_ADDRESS)")
	fs.StringVar(&cfg.logLevel, "log-level", env("LOG_LEVEL", "info"),
		"log level: debug, info, warning or error (env LOG_LEVEL)")
	fs.StringVar(&cfg.logFormat, "log-format", env("LOG_FORMAT", "text"),
		"log format: text or json (env LOG_FORMAT)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	if len(cfg.domainFilter) == 0 {
		for _, value := range strings.Split(getenv("DOMAIN_FILTER"), ",") {
			if value = strings.TrimSpace(value); value != "" {
				cfg.domainFilter = append(cfg.domainFilter, value)
			}
		}
	}

	return cfg, nil
}

func configureLogger(cfg *config) error {
	level, err := log.ParseLevel(cfg.logLevel)
	if err != nil {
		return err
	}
	log.SetLevel(level)

	switch cfg.logFormat {
	case "text":
		log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	case "json":
		log.SetFormatter(&log.JSONFormatter{})
	default:
		return fmt.Errorf("unknown log format %q", cfg.logFormat)
	}

	return nil
}

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		log.Fatal(err)
	}
	if err := configureLogger(cfg); err != nil {
		log.Fatal(err)
	}

	provider, err := transip.New(cfg.account, cfg.keyFile, cfg.timeout, endpoint.NewDomainFilter(cfg.domainFilter), cfg.dryRun)
	if err != nil {
		log.Fatal(err)
	}
	if cfg.dryRun {
		log.Info("running in dry-run mode. No changes to DNS records will be made.")
	}

	// The API server exits the process when it cannot listen or stops serving
	var ready atomic.Bool
	started := make(chan struct{})
	go api.StartHTTPApi(api.ServerOptions{
		Provider:          provider,
		StartedChan:       started,
		ProviderPort:      cfg.listenAddress,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		// applying a large change is one TransIP API call per DNS entry
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	})
	go func() {
		<-started
		ready.Store(true)
		log.Infof("webhook API listening on %s", cfg.listenAddress)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	listener, err := net.Listen("tcp", cfg.healthAddress)
	if err != nil {
		log.Fatal(err)
	}
	log.Infof("health endpoint listening on %s", cfg.healthAddress)

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.Serve(listener))
}
