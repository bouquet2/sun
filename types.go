package main

import (
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const version = "0.1.4"

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

	// Resource monitoring configuration
	ResourceMonitoring ResourceMonitoringConfig `mapstructure:"resource_monitoring"`

	// Node monitoring configuration
	NodeMonitoring NodeMonitoringConfig `mapstructure:"node_monitoring"`

	// Longhorn configuration
	Longhorn LonghornConfig `mapstructure:"longhorn"`

	// GitOps configuration
	GitOps GitOpsConfig `mapstructure:"gitops"`
}

type ResourceMonitoringConfig struct {
	Enabled  bool                       `mapstructure:"enabled"` // Default: true
	Denylist ResourceMonitoringDenylist `mapstructure:"denylist"`
}

type ResourceMonitoringDenylist struct {
	Kinds []string `mapstructure:"kinds"` // Default: empty list
}

type NodeMonitoringConfig struct {
	Enabled             bool    `mapstructure:"enabled"`               // Default: true
	CPUThresholdPercent float64 `mapstructure:"cpu_threshold_percent"` // Default: 80%
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

type GitOpsConfig struct {
	Enabled             bool               `mapstructure:"enabled"`               // Default: false
	AlertOnMismatch     bool               `mapstructure:"alert_on_mismatch"`     // Default: true
	SyncIntervalMinutes int                `mapstructure:"sync_interval_minutes"` // Default: 5 minutes
	AutoFix             GitOpsAutoFix      `mapstructure:"auto_fix"`
	Allowlist           GitOpsFilter       `mapstructure:"allowlist"`
	Denylist            GitOpsFilter       `mapstructure:"denylist"`
	Repositories        []GitOpsRepository `mapstructure:"repositories"`
}

type GitOpsAutoFix struct {
	Enabled bool     `mapstructure:"enabled"` // Default: false
	Kinds   []string `mapstructure:"kinds"`   // Default: empty list
}

type GitOpsFilter struct {
	Namespaces []string `mapstructure:"namespaces"` // Default: empty list
	Kinds      []string `mapstructure:"kinds"`      // Default: empty list
}

type GitOpsRepository struct {
	Name                string                `mapstructure:"name"`
	URL                 string                `mapstructure:"url"`
	Path                string                `mapstructure:"path"`                  // Default: "."
	Branch              string                `mapstructure:"branch"`                // Default: "main"
	AlertOnMismatch     bool                  `mapstructure:"alert_on_mismatch"`     // Default: true
	AutoFix             bool                  `mapstructure:"auto_fix"`              // Default: false
	SyncIntervalMinutes int                   `mapstructure:"sync_interval_minutes"` // Default: use global setting
	Kustomize           GitOpsKustomizeConfig `mapstructure:"kustomize"`
}

type GitOpsKustomizeConfig struct {
	HelmCommand    string `mapstructure:"helmCommand"`    // Default: "helm"
	CopyEnvExample bool   `mapstructure:"copyEnvExample"` // Default: false
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

// Node-specific state for resource monitoring
type nodeResourceState struct {
	unitState
	cpuCapacity     int64
	cpuRequests     int64
	cpuUsagePercent float64
	nodeName        string
}

// GitOps-specific state
type gitOpsState struct {
	unitState
	repositoryName string
	resourceKind   string
	resourceName   string
	namespace      string
	mismatchType   string // "missing", "different", "extra"
	expectedHash   string
	actualHash     string
}

type gitOpsRepositoryState struct {
	name         string
	url          string
	path         string
	branch       string
	localPath    string
	repository   *git.Repository
	lastSync     time.Time
	lastCommit   string
	syncInterval time.Duration
	mutex        sync.RWMutex
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

	// Node resource monitoring state
	nodeResourceStates     = make(map[string]nodeResourceState)
	nodeResourceStatesLock sync.RWMutex

	// GitOps state maps
	gitOpsStates     = make(map[string]gitOpsState)
	gitOpsStatesLock sync.RWMutex
)
