package main

import (
	"sync"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const version = "0.0.11"

var isLeader bool
var leaderLock sync.RWMutex
var config Config
var client *kubernetes.Clientset
var dynamicClient dynamic.Interface

type Config struct {
	WebhookUrl string `mapstructure:"webhook_url"`
	Namespace  string `mapstructure:"namespace"`
	LogLevel   string `mapstructure:"log_level"`
	Interval   int    `mapstructure:"interval"` // Interval in minutes

	// Longhorn configuration
	Longhorn LonghornConfig `mapstructure:"longhorn"`
}

type LonghornConfig struct {
	Enabled         bool               `mapstructure:"enabled"`
	Namespace       string             `mapstructure:"namespace"` // Default: "longhorn-system"
	Monitor         LonghornMonitor    `mapstructure:"monitor"`
	AlertThresholds LonghornThresholds `mapstructure:"alert_thresholds"`
}

type LonghornMonitor struct {
	Volumes  bool `mapstructure:"volumes"`
	Replicas bool `mapstructure:"replicas"`
	Engines  bool `mapstructure:"engines"`
	Nodes    bool `mapstructure:"nodes"`
	Backups  bool `mapstructure:"backups"`
}

type LonghornThresholds struct {
	VolumeUsagePercent     float64 `mapstructure:"volume_usage_percent"`     // Default: 85%
	VolumeCapacityCritical int64   `mapstructure:"volume_capacity_critical"` // Default: 1GB remaining
	ReplicaFailureCount    int     `mapstructure:"replica_failure_count"`    // Default: 1
}

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

type unitState struct {
	hasError    bool
	lastSeen    time.Time
	lastMessage string
	firstError  time.Time // When the error was first seen
	alertSent   bool      // Whether we've sent an alert for the current error state
}

// Longhorn-specific state
type longhornUnitState struct {
	unitState
	resourceType string // "volume", "replica", "engine", etc.
	capacity     int64
	usage        int64
	robustness   string
	node         string
	namespace    string
}

// Longhorn state maps
var (
	longhornVolumeStates  = make(map[string]longhornUnitState)
	longhornReplicaStates = make(map[string]longhornUnitState)
	longhornEngineStates  = make(map[string]longhornUnitState)
	longhornNodeStates    = make(map[string]longhornUnitState)
	longhornBackupStates  = make(map[string]longhornUnitState)

	longhornVolumeStatesLock  sync.RWMutex
	longhornReplicaStatesLock sync.RWMutex
	longhornEngineStatesLock  sync.RWMutex
	longhornNodeStatesLock    sync.RWMutex
	longhornBackupStatesLock  sync.RWMutex
)
