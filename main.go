package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
		if field.Name == "State" && field.Value == "Running" {
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
				"text": "moniquet v%s",
				"icon_url": "https://raw.githubusercontent.com/kreatoo/moniquet/refs/heads/main/karpuz.png"
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
	action := "Loading"
	if isReload {
		action = "Reloading"
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
	log.Info().Str("version", version).Msg("Starting moniquet")
	// Initialize Viper
	viper.SetConfigName("config")         // name of config file (without extension)
	viper.AddConfigPath(".")              // path to look for the config file in
	viper.SetConfigType("yaml")           // type of the config file
	viper.SetDefault("log_level", "info") // Set default log level to info
	viper.SetDefault("interval", 3)       // Set default interval to 3 minutes

	// Enable config watching
	viper.WatchConfig()
	viper.OnConfigChange(func(e fsnotify.Event) {
		log.Info().Str("file", e.Name).Msg("Config file changed")
		loadConfig(true)
	})

	// Human friendly logging
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}).With().Caller().Logger()

	// Load initial configuration
	loadConfig(false)

	// Initialize Kubernetes client
	var k8sConfig *rest.Config
	var err error

	// Try to get in-cluster config first
	k8sConfig, err = rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig if not running in cluster
		kubeconfig := clientcmd.NewDefaultClientConfigLoadingRules().GetDefaultFilename()
		log.Debug().Str("kubeconfig", kubeconfig).Msg("Loading kubeconfig")
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Error().Err(err).Msg("Failed to build kubeconfig")
			return
		}
	}

	client, err = kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create Kubernetes client")
		return
	}
	log.Debug().Msg("Successfully initialized Kubernetes client")

	// Create a context for leader election
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start leader election in a goroutine
	go runLeaderElection(ctx)

	// Start node and pod watching
	go checkNodes()
	checkPods()
}
