package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

func shouldSendAlert(alertType string, key string) bool {
	var state unitState
	var exists bool

	switch alertType {
	case "pod":
		podStatesLock.RLock()
		defer podStatesLock.RUnlock()
		state, exists = podStates[key]
	case "node":
		nodeStatesLock.RLock()
		defer nodeStatesLock.RUnlock()
		state, exists = nodeStates[key]
	case "node_resource":
		nodeResourceStatesLock.RLock()
		defer nodeResourceStatesLock.RUnlock()
		if nodeState, ok := nodeResourceStates[key]; ok {
			state = nodeState.unitState
			exists = true
		}
	case "gitops":
		gitOpsStatesLock.RLock()
		defer gitOpsStatesLock.RUnlock()
		if gitOpsState, ok := gitOpsStates[key]; ok {
			state = gitOpsState.unitState
			exists = true
		}
	}

	if !exists || !state.hasError || state.alertSent {
		return false
	}

	// If interval is 0, send alert immediately
	if config.Interval == 0 {
		return true
	}

	// Check if enough time has passed since the error was first seen
	intervalDuration := time.Duration(config.Interval) * time.Minute
	return time.Since(state.firstError) >= intervalDuration
}

func markAlertSent(podKey string) {
	podStatesLock.Lock()
	defer podStatesLock.Unlock()

	if state, exists := podStates[podKey]; exists {
		state.alertSent = true
		podStates[podKey] = state
	}
}

func sendWebhookMessage(alert Alert) {
	leaderLock.RLock()
	if !isLeader {
		leaderLock.RUnlock()
		log.Debug().Msg("Not the leader, skipping webhook message")
		return
	}
	leaderLock.RUnlock()

	log.Debug().Str("title", alert.Title).Msg("Sending webhook message")

	// Set color and emoji based on state
	color := 16711680 // Default to red for errors
	emoji := "🔴"      // Default to red circle for errors
	for _, field := range alert.Fields {
		if (field.Name == "State" && (field.Value == "Running" || field.Value == "Completed")) ||
			(field.Name == "Status" && field.Value == "✅ In Sync") {
			color = 65280 // Green for success
			emoji = "🟢"   // Green circle for success
			break
		}
	}

	// Add emoji to title
	alert.Title = emoji + " " + alert.Title

	// Convert fields to JSON array
	fieldsJSON := "["
	for i, field := range alert.Fields {
		if i > 0 {
			fieldsJSON += ","
		}
		fieldsJSON += fmt.Sprintf(`{"name":"%s","value":"%s","inline":%t}`, field.Name, field.Value, field.Inline)
	}
	fieldsJSON += "]"

	// Add logs field if available
	if alert.Logs != "" {
		fieldsJSON = fieldsJSON[:len(fieldsJSON)-1] // Remove last ]
		fieldsJSON += fmt.Sprintf(`,{"name":"Container Logs","value":"%s","inline":false}]`, alert.Logs)
	}

	// Create JSON payload with Discord embed
	jsonPayload := fmt.Sprintf(`{
		"embeds": [{
			"title": "%s",
			"description": "%s",
			"color": %d,
			"fields": %s,
			"timestamp": "%s",
			"footer": {
				"text": "sun v%s",
				"icon_url": "https://avatars.githubusercontent.com/u/221393700"
			}
		}]
	}`, alert.Title, alert.Description, color, fieldsJSON, time.Now().Format(time.RFC3339), version)

	// Create HTTP request
	req, err := http.NewRequest("POST", config.WebhookUrl, bytes.NewBufferString(jsonPayload))
	if err != nil {
		log.Error().Err(err).Msg("Failed to create HTTP request")
		return
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")

	// Send request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Error().Err(err).Msg("Failed to send HTTP request")
		return
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Error().Int("status_code", resp.StatusCode).Msg("Webhook request failed")
	} else {
		log.Debug().Int("status_code", resp.StatusCode).Msg("Webhook message sent successfully")
	}
}

func loadConfig(isReload bool) {
	action := "Load"
	if isReload {
		action = "Reload"
	}
	log.Info().Msg(action + " configuration...")

	// Read the configuration file
	if err := viper.ReadInConfig(); err != nil {
		log.Error().Err(err).Msg("Error reading config file")
		return
	}

	// Unmarshal the configuration into a Config struct
	if err := viper.Unmarshal(&config); err != nil {
		log.Error().Err(err).Msg("Unable to decode into struct")
		return
	}

	// Apply defaults for GitOps repository settings
	// Since we can't distinguish between explicitly set false and default false,
	// we'll use a different approach: check the raw config to see if alert_on_mismatch was set
	for i := range config.GitOps.Repositories {
		repo := &config.GitOps.Repositories[i]

		// Check if alert_on_mismatch was explicitly set for this repository
		repoKey := fmt.Sprintf("gitops.repositories.%d.alert_on_mismatch", i)
		if !viper.IsSet(repoKey) {
			// Not explicitly set, use global default
			repo.AlertOnMismatch = config.GitOps.AlertOnMismatch
			log.Debug().
				Str("repository", repo.Name).
				Bool("alertOnMismatch", repo.AlertOnMismatch).
				Msg("Applied global AlertOnMismatch default for repository")
		}
	}

	// Check for WEBHOOK_URL environment variable
	if webhookUrl := os.Getenv("WEBHOOK_URL"); webhookUrl != "" {
		config.WebhookUrl = webhookUrl
		log.Info().Msg("Using webhook URL from environment variable")
	}

	// Set log level based on configuration
	level, err := zerolog.ParseLevel(config.LogLevel)
	if err != nil {
		log.Error().Err(err).Msg("Invalid log level, defaulting to info")
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	log.Info().
		Str("namespace", config.Namespace).
		Str("log_level", config.LogLevel).
		Int("interval", config.Interval).
		Bool("resource_monitoring_enabled", config.ResourceMonitoring.Enabled).
		Int("resource_monitoring_denylist_kinds_count", len(config.ResourceMonitoring.Denylist.Kinds)).
		Bool("node_monitoring_enabled", config.NodeMonitoring.Enabled).
		Float64("cpu_threshold_percent", config.NodeMonitoring.CPUThresholdPercent).
		Bool("longhorn_enabled", config.Longhorn.Enabled).
		Str("longhorn_namespace", config.Longhorn.Namespace).
		Bool("gitops_enabled", config.GitOps.Enabled).
		Bool("gitops_alert_on_mismatch", config.GitOps.AlertOnMismatch).
		Int("gitops_sync_interval_minutes", config.GitOps.SyncIntervalMinutes).
		Bool("gitops_auto_fix_enabled", config.GitOps.AutoFix.Enabled).
		Int("gitops_repositories_count", len(config.GitOps.Repositories)).
		Msg("Configuration " + strings.ToLower(action) + "ed")
}

func detectNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return strings.TrimSpace(string(data))
	}
	return "default"
}

func main() {
	// Silence klog for leader election noise
	klog.InitFlags(nil)
	flag.Set("logtostderr", "false")
	flag.Set("alsologtostderr", "false")
	flag.Parse()

	// Create context that cancels on OS signals for graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Initialize Viper
	viper.SetConfigName("config")         // name of config file (without extension)
	viper.AddConfigPath(".")              // path to look for the config file in
	viper.SetConfigType("yaml")           // type of the config file
	viper.SetDefault("log_level", "info") // Set default log level to info
	viper.SetDefault("interval", 3)       // Set default interval to 3 minutes

	// Set resource monitoring defaults
	viper.SetDefault("resource_monitoring.enabled", true)
	viper.SetDefault("resource_monitoring.denylist.kinds", []string{})

	// Set node monitoring defaults
	viper.SetDefault("node_monitoring.enabled", true)
	viper.SetDefault("node_monitoring.cpu_threshold_percent", 80.0)

	// Set Longhorn defaults
	viper.SetDefault("longhorn.enabled", false)
	viper.SetDefault("longhorn.namespace", "longhorn-system")
	viper.SetDefault("longhorn.monitor.volumes", true)
	viper.SetDefault("longhorn.monitor.replicas", true)
	viper.SetDefault("longhorn.monitor.engines", true)
	viper.SetDefault("longhorn.monitor.nodes", true)
	viper.SetDefault("longhorn.monitor.backups", true)
	viper.SetDefault("longhorn.alert_thresholds.volume_usage_percent", 85.0)
	viper.SetDefault("longhorn.alert_thresholds.volume_capacity_critical", 1073741824)
	viper.SetDefault("longhorn.alert_thresholds.replica_failure_count", 1)

	// Set GitOps defaults
	viper.SetDefault("gitops.enabled", false)
	viper.SetDefault("gitops.alert_on_mismatch", true)
	viper.SetDefault("gitops.sync_interval_minutes", 5)
	viper.SetDefault("gitops.auto_fix.enabled", false)

	// Set default Kustomize options for all repositories
	viper.SetDefault("gitops.repositories.kustomize.copyEnvExample", false)

	// Enable config watching
	viper.WatchConfig()
	viper.OnConfigChange(func(e fsnotify.Event) {
		log.Info().Str("file", e.Name).Msg("Config file changed")
		loadConfig(true)
	})

	// Human friendly logging
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}).With().Caller().Logger()

	fmt.Println(`
  ________ __  ____
 /  ___/  |  \/    \
 \___ \|  |  /   |  \
/____  >____/|___|  /
     \/           \/`)

	log.Info().Str("version", version).Msg("Starting sun")

	// Silence klog (e.g. client-go and controller-runtime internal logs)
	klog.SetOutput(io.Discard)

	// Load initial configuration
	loadConfig(false)

	// Initialize Kubernetes client
	var k8sConfig *rest.Config
	var err error
	var runningInCluster bool

	// Try to get in-cluster config first
	k8sConfig, err = rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig if not running in cluster
		runningInCluster = false
		kubeconfig := clientcmd.NewDefaultClientConfigLoadingRules().GetDefaultFilename()
		log.Debug().Str("kubeconfig", kubeconfig).Msg("Loading kubeconfig")
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Error().Err(err).Msg("Failed to build kubeconfig")
			return
		}
	} else {
		runningInCluster = true
	}

	client, err = kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create Kubernetes client")
		return
	}
	log.Debug().Msg("Successfully initialized Kubernetes client")

	// Initialize dynamic client for Longhorn CRDs
	dynamicClient, err = dynamic.NewForConfig(k8sConfig)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create dynamic client")
		return
	}
	log.Debug().Msg("Successfully initialized dynamic client")

	// Start leader election only if running in cluster
	if runningInCluster {
		log.Info().Msg("Running in cluster, starting leader election")
		go runLeaderElection(ctx)
	} else {
		log.Info().Msg("Running outside cluster, skipping leader election and assuming leadership")
		// Set as leader immediately when not in cluster
		leaderLock.Lock()
		isLeader = true
		leaderLock.Unlock()
	}

	// Create SharedInformerFactory
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(config.Namespace))

	// Set up pod informer
	podInformer := factory.Core().V1().Pods().Informer()
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    handlePod,
		UpdateFunc: func(_, obj interface{}) { handlePod(obj) },
	})

	// Set up node informer (cluster-wide)
	nodeInformer := factory.Core().V1().Nodes().Informer()
	nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    handleNode,
		UpdateFunc: func(_, obj interface{}) { handleNode(obj) },
	})

	// Start informers
	log.Info().Msg("Starting SharedInformerFactory")
	go factory.Start(ctx.Done())

	// Wait for cache sync
	log.Info().Msg("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced, nodeInformer.HasSynced) {
		log.Error().Msg("Failed to sync informer caches")
		return
	}
	log.Info().Msg("Informer caches synced successfully")

	// Setup Longhorn monitoring if enabled
	if config.Longhorn.Enabled {
		err = setupLonghornInformers(ctx)
		if err != nil {
			log.Error().Err(err).Msg("Failed to setup Longhorn informers")
			// Don't exit, continue with pod/node monitoring
		}
	}

	// Setup GitOps monitoring if enabled
	if config.GitOps.Enabled {
		err = setupGitOpsMonitoring(ctx)
		if err != nil {
			log.Error().Err(err).Msg("Failed to setup GitOps monitoring")
			// Don't exit, continue with other monitoring
		}
	}

	// Block until context is cancelled (signal received)
	<-ctx.Done()
	log.Info().Msg("Shutting down sun")
}

// handlePod processes pod events from the informer
func handlePod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		log.Error().Msg("Received non-pod object in pod informer")
		return
	}

	log.Debug().
		Str("pod", pod.Name).
		Str("namespace", pod.Namespace).
		Str("phase", string(pod.Status.Phase)).
		Msg("Processing pod status")

	hasError := false
	var errorMessage string

	for _, container := range pod.Status.ContainerStatuses {
		containerHasError, containerErrorMessage := processContainerStatus(pod, container)
		if containerHasError {
			hasError = true
			errorMessage = containerErrorMessage
		}
	}

	updatePodState(pod, hasError, errorMessage)
}

// handleNode processes node events from the informer
func handleNode(obj interface{}) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		log.Error().Msg("Received non-node object in node informer")
		return
	}

	log.Debug().
		Str("node", node.Name).
		Msg("Processing node status")

	hasError, errorMessage := processNodeStatus(node)
	updateNodeState(node, hasError, errorMessage)

	// Also check resource usage if node monitoring is enabled
	processNodeResourceUsage(node.Name)
}
