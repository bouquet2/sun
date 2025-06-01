package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const version = "0.0.5"

type Config struct {
	WebhookUrl string `mapstructure:"webhook_url"`
	Namespace  string `mapstructure:"namespace"`
	LogLevel   string `mapstructure:"log_level"`
	Interval   int    `mapstructure:"interval"` // Interval in minutes
}

var config Config
var client *kubernetes.Clientset
var isLeader bool
var leaderLock sync.RWMutex

type Alert struct {
	Title       string
	Description string
	Fields      []struct {
		Name   string
		Value  string
		Inline bool
	}
	Logs string // Add logs field
}

// Add podStateTracker to track pod states
type podState struct {
	hasError    bool
	lastSeen    time.Time
	lastMessage string
	firstError  time.Time // When the error was first seen
	alertSent   bool      // Whether we've sent an alert for the current error state
}

var (
	podStates     = make(map[string]podState)
	podStatesLock sync.RWMutex
)

func setupPodWatcher() (watch.Interface, error) {
	log.Debug().Str("namespace", config.Namespace).Msg("Starting pod watch")
	watcher, err := client.CoreV1().Pods(config.Namespace).Watch(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Error().Err(err).Msg("Failed to watch pods")
		return nil, err
	}
	log.Debug().Msg("Successfully started pod watch")
	return watcher, nil
}

func getContainerLogs(pod *corev1.Pod, containerName string) string {
	req := client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &[]int64{50}[0], // Get last 50 lines
	})

	logs, err := req.DoRaw(context.Background())
	if err != nil {
		log.Error().Err(err).
			Str("pod", pod.Name).
			Str("container", containerName).
			Msg("Failed to fetch container logs")
		return "Failed to fetch logs"
	}

	return string(logs)
}

func handleTerminatedContainer(pod *corev1.Pod, container corev1.ContainerStatus) {
	containerLogs := getContainerLogs(pod, container.Name)

	alert := Alert{
		Title:       fmt.Sprintf("Pod Failure on %s", pod.Namespace),
		Description: fmt.Sprintf("Pod %s in namespace %s has failed", pod.Name, pod.Namespace),
		Fields: []struct {
			Name   string
			Value  string
			Inline bool
		}{
			{
				Name:   "Container",
				Value:  container.Name,
				Inline: true,
			},
			{
				Name:   "State",
				Value:  "Terminated",
				Inline: true,
			},
			{
				Name:   "Exit Code",
				Value:  fmt.Sprintf("%d", container.State.Terminated.ExitCode),
				Inline: true,
			},
			{
				Name:   "Reason",
				Value:  container.State.Terminated.Reason,
				Inline: true,
			},
		},
		Logs: containerLogs,
	}
	sendWebhookMessage(alert)
	log.Error().
		Str("pod", pod.Name).
		Str("namespace", pod.Namespace).
		Str("container", container.Name).
		Int32("exit_code", container.State.Terminated.ExitCode).
		Str("reason", container.State.Terminated.Reason).
		Msg("Pod has failed")
}

func handleWaitingContainer(pod *corev1.Pod, container corev1.ContainerStatus) {
	var containerLogs string
	if container.State.Waiting != nil && container.State.Waiting.Reason == "PodInitializing" {
		containerLogs = "Pod is initializing - logs not available yet"
	} else {
		containerLogs = getContainerLogs(pod, container.Name)
	}

	alert := Alert{
		Title:       fmt.Sprintf("Pod Waiting on %s", pod.Namespace),
		Description: fmt.Sprintf("Pod %s in namespace %s is waiting", pod.Name, pod.Namespace),
		Fields: []struct {
			Name   string
			Value  string
			Inline bool
		}{
			{
				Name:   "Container",
				Value:  container.Name,
				Inline: true,
			},
			{
				Name:   "State",
				Value:  container.State.Waiting.Reason,
				Inline: true,
			},
			{
				Name:   "Reason",
				Value:  container.State.Waiting.Reason,
				Inline: true,
			},
		},
		Logs: containerLogs,
	}
	sendWebhookMessage(alert)
	log.Error().
		Str("pod", pod.Name).
		Str("namespace", pod.Namespace).
		Str("container", container.Name).
		Str("reason", container.State.Waiting.Reason).
		Msg("Pod is waiting")
}

func handleRunningContainer(pod *corev1.Pod, container corev1.ContainerStatus) {
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	podStatesLock.RLock()
	prevState, exists := podStates[podKey]
	podStatesLock.RUnlock()

	if exists && prevState.hasError {
		alert := Alert{
			Title:       "Pod Recovery Alert",
			Description: fmt.Sprintf("Pod %s in namespace %s has recovered", pod.Name, pod.Namespace),
			Fields: []struct {
				Name   string
				Value  string
				Inline bool
			}{
				{
					Name:   "Container",
					Value:  container.Name,
					Inline: true,
				},
				{
					Name:   "State",
					Value:  "Running",
					Inline: true,
				},
			},
		}
		sendWebhookMessage(alert)
	}
}

func updatePodState(pod *corev1.Pod, hasError bool, errorMessage string) {
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	podStatesLock.Lock()
	defer podStatesLock.Unlock()

	now := time.Now()
	prevState, exists := podStates[podKey]

	if !exists {
		// New pod state
		podStates[podKey] = podState{
			hasError:    hasError,
			lastSeen:    now,
			lastMessage: errorMessage,
			firstError:  now,
			alertSent:   false,
		}
		return
	}

	// Update existing state
	if hasError && !prevState.hasError {
		// Error just started
		prevState.firstError = now
		prevState.alertSent = false
	} else if !hasError {
		// Error resolved
		prevState.firstError = time.Time{}
		prevState.alertSent = false
	}

	prevState.hasError = hasError
	prevState.lastSeen = now
	prevState.lastMessage = errorMessage
	podStates[podKey] = prevState
}

func shouldSendAlert(podKey string) bool {
	podStatesLock.RLock()
	defer podStatesLock.RUnlock()

	state, exists := podStates[podKey]
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

func processContainerStatus(pod *corev1.Pod, container corev1.ContainerStatus) (bool, string) {
	log.Debug().
		Str("container", container.Name).
		Str("state", fmt.Sprintf("%+v", container.State)).
		Msg("Checking container status")

	hasError := false
	var errorMessage string
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	if container.State.Terminated != nil && container.State.Terminated.ExitCode != 0 {
		hasError = true
		errorMessage = fmt.Sprintf("Container %s has failed", container.Name)
		if shouldSendAlert(podKey) {
			handleTerminatedContainer(pod, container)
			markAlertSent(podKey)
		}
	}

	if container.State.Waiting != nil {
		hasError = true
		errorMessage = fmt.Sprintf("Container %s is waiting", container.Name)
		if shouldSendAlert(podKey) {
			handleWaitingContainer(pod, container)
			markAlertSent(podKey)
		}
	}

	if container.State.Running != nil {
		podStatesLock.RLock()
		state, exists := podStates[podKey]
		podStatesLock.RUnlock()

		if exists && state.hasError && state.alertSent {
			handleRunningContainer(pod, container)
		}
	}

	return hasError, errorMessage
}

func checkPods() {
	watcher, err := setupPodWatcher()
	if err != nil {
		return
	}

	for event := range watcher.ResultChan() {
		log.Debug().Str("event_type", string(event.Type)).Msg("Received pod event")
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			log.Error().Msg("Received non-pod object in watch")
			continue
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

func runLeaderElection(ctx context.Context) {
	// Get the pod name from environment variable
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		log.Error().Msg("POD_NAME environment variable not set")
		return
	}

	// Create a new lock
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "moniquet-leader",
			Namespace: config.Namespace,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: podName,
		},
	}

	// Start the leader election code loop
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				leaderLock.Lock()
				isLeader = true
				leaderLock.Unlock()
				log.Info().Msg("Started leading")
			},
			OnStoppedLeading: func() {
				leaderLock.Lock()
				isLeader = false
				leaderLock.Unlock()
				log.Info().Msg("Stopped leading")
			},
			OnNewLeader: func(identity string) {
				log.Info().Str("leader", identity).Msg("New leader elected")
			},
		},
	})
}

func main() {
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

	// Start pod watching
	checkPods()
}
