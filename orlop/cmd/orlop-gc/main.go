package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/stdr"
	_ "github.com/lib/pq"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/gc"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/postgres"

	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
)

func main() {
	var (
		interval       time.Duration
		eventRetention time.Duration
		dbHost         string
		dbPort         int
		dbName         string
		dbUser         string
		dbPassword     string
		dbSSLMode      string
		verbosity      int
	)

	flag.DurationVar(&interval, "interval", 30*time.Second, "garbage collection interval")
	flag.DurationVar(&eventRetention, "event-retention", 24*time.Hour, "how long to retain resource events before pruning")
	flag.StringVar(&dbHost, "db-host", "localhost", "PostgreSQL host")
	flag.IntVar(&dbPort, "db-port", 5432, "PostgreSQL port")
	flag.StringVar(&dbName, "db-name", "orlop", "PostgreSQL database name")
	flag.StringVar(&dbUser, "db-user", "orlop", "PostgreSQL user")
	flag.StringVar(&dbPassword, "db-password", "", "PostgreSQL password")
	flag.StringVar(&dbSSLMode, "db-sslmode", "disable", "PostgreSQL SSL mode")
	flag.IntVar(&verbosity, "v", 0, "log verbosity level")
	flag.Parse()

	// Setup logger
	stdr.SetVerbosity(verbosity)
	logger := stdr.New(nil)

	logger.Info("Starting Orlop garbage collector",
		"interval", interval,
		"dbHost", dbHost,
		"dbPort", dbPort,
		"dbName", dbName)

	// Get resource definitions
	resources := getResources()
	scheme := getScheme()

	// Connect to PostgreSQL
	connStr := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s",
		dbHost, dbPort, dbName, dbUser, dbPassword, dbSSLMode)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		logger.Error(err, "Failed to connect to database")
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		logger.Error(err, "Failed to ping database")
		os.Exit(1)
	}

	logger.Info("Connected to database")

	// Create stores and broadcasters for all resources
	ctx := context.Background()
	stores := make(map[runtimeschema.GroupKind]storage.ResourceStore)
	var broadcasters []storage.EventBroadcaster
	for _, res := range resources {
		resourceType := strings.ToLower(res.GVK.Group + "_" + res.GVK.Kind)
		broadcaster, err := postgres.NewPostgresBroadcaster(ctx, postgres.PostgresBroadcasterConfig{
			DB:          db,
			ConnString:  connStr,
			ChannelName: "events_" + resourceType,
			TableName:   "event_log_" + resourceType,
			Scheme:      scheme,
			GVK:         res.GVK,
		})
		if err != nil {
			logger.Error(err, "Failed to create broadcaster", "groupKind", res.GVK.GroupKind())
			os.Exit(1)
		}
		broadcasters = append(broadcasters, broadcaster)

		config := postgres.PostgresStoreConfig{
			DB:           db,
			ResourceType: resourceType,
			Scheme:       scheme,
			GVK:          res.GVK,
			Broadcaster:  broadcaster,
		}

		store, err := postgres.NewPostgresStore(ctx, config)
		if err != nil {
			logger.Error(err, "Failed to create store", "groupKind", res.GVK.GroupKind())
			os.Exit(1)
		}
		stores[res.GVK.GroupKind()] = store
		logger.Info("Created store for resource", "groupKind", res.GVK.GroupKind())
	}

	// Create and start garbage collector
	collector := gc.NewCollector(stores, interval, logger)
	collector.SetBroadcasters(broadcasters)
	collector.SetEventRetention(eventRetention)

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		logger.Info("Received shutdown signal")
		cancel()
		collector.Stop()
	}()

	// Run garbage collector
	logger.Info("Garbage collector running")
	collector.Start(ctx)

	logger.Info("Garbage collector stopped")
}

// getResources returns the list of resources to garbage collect.
// In a real implementation, this would be loaded from configuration or discovery.
func getResources() []ResourceInfo {
	return []ResourceInfo{
		{
			Plural: "objects",
			GVK: runtimeschema.GroupVersionKind{
				Group:   "test.orlop.gcp.managed.openshift.io",
				Version: "v1",
				Kind:    "Object",
			},
		},
		{
			Plural: "others",
			GVK: runtimeschema.GroupVersionKind{
				Group:   "test.orlop.gcp.managed.openshift.io",
				Version: "v1",
				Kind:    "Other",
			},
		},
	}
}

// getScheme returns the runtime scheme with registered types.
// In a real implementation, this would include all API types.
func getScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	// Register types here
	// For now, we rely on the stores to handle unstructured objects
	return scheme
}

// ResourceInfo holds information about a resource type.
type ResourceInfo struct {
	Plural string
	GVK    runtimeschema.GroupVersionKind
}
