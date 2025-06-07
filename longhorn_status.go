package main

import (
	"fmt"
	"time"

	log "github.com/rs/zerolog/log"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// processLonghornVolumeStatus processes the status of a Longhorn volume
func processLonghornVolumeStatus(name, namespace, state, robustness string, capacity, actualSize int64) {
	key := fmt.Sprintf("%s/%s", namespace, name)

	log.Debug().
		Str("volume", name).
		Str("namespace", namespace).
		Str("state", state).
		Str("robustness", robustness).
		Int64("capacity", capacity).
		Int64("actualSize", actualSize).
		Msg("Processing volume status")

	hasError := false
	var errorMessage string
	var alertType string

	// Check for volume state issues
	switch state {
	case "detached", "attached":
		// Normal states, check robustness
		switch robustness {
		case "healthy":
			// Volume is healthy
		case "degraded":
			hasError = true
			errorMessage = "Volume is degraded"
			alertType = "degraded"
		case "faulted":
			hasError = true
			errorMessage = "Volume is faulted"
			alertType = "faulted"
		}
	case "creating", "attaching", "detaching":
		// Transitional states, generally OK but monitor
		log.Debug().Str("volume", name).Str("state", state).Msg("Volume in transitional state")
	default:
		// Unknown state
		hasError = true
		errorMessage = fmt.Sprintf("Volume in unknown state: %s", state)
		alertType = "unknown_state"
	}

	// Check for volume capacity issues
	if capacity > 0 && actualSize > 0 {
		usagePercent := float64(actualSize) / float64(capacity) * 100
		remaining := capacity - actualSize

		if usagePercent > config.Longhorn.AlertThresholds.VolumeUsagePercent {
			hasError = true
			errorMessage = fmt.Sprintf("Volume usage critical: %.1f%% used", usagePercent)
			alertType = "usage_critical"
		} else if remaining < config.Longhorn.AlertThresholds.VolumeCapacityCritical {
			hasError = true
			errorMessage = fmt.Sprintf("Volume capacity critical: %d bytes remaining", remaining)
			alertType = "capacity_critical"
		}
	}

	// Update state and send alerts
	updateLonghornVolumeState(key, hasError, errorMessage, state, robustness, capacity, actualSize, namespace)

	if hasError && shouldSendLonghornAlert("volume", key) {
		sendLonghornVolumeAlert(name, namespace, state, robustness, capacity, actualSize, errorMessage, alertType)
		markLonghornAlertSent("volume", key)
	} else if !hasError {
		// Check for recovery
		checkLonghornVolumeRecovery(key, name, namespace)
	}
}

// processLonghornReplicaStatus processes the status of a Longhorn replica
func processLonghornReplicaStatus(name, namespace, currentState string) {
	key := fmt.Sprintf("%s/%s", namespace, name)

	log.Debug().
		Str("replica", name).
		Str("namespace", namespace).
		Str("state", currentState).
		Msg("Processing replica status")

	hasError := false
	var errorMessage string

	// Check replica state
	switch currentState {
	case "running":
		// Healthy state
	case "stopped", "error":
		hasError = true
		errorMessage = fmt.Sprintf("Replica in %s state", currentState)
	case "starting", "stopping":
		// Transitional states
		log.Debug().Str("replica", name).Str("state", currentState).Msg("Replica in transitional state")
	default:
		hasError = true
		errorMessage = fmt.Sprintf("Replica in unknown state: %s", currentState)
	}

	// Update state and send alerts
	updateLonghornReplicaState(key, hasError, errorMessage, currentState, namespace)

	if hasError && shouldSendLonghornAlert("replica", key) {
		sendLonghornReplicaAlert(name, namespace, currentState, errorMessage)
		markLonghornAlertSent("replica", key)
	} else if !hasError {
		checkLonghornReplicaRecovery(key, name, namespace)
	}
}

// processLonghornEngineStatus processes the status of a Longhorn engine
func processLonghornEngineStatus(name, namespace, currentState string) {
	key := fmt.Sprintf("%s/%s", namespace, name)

	log.Debug().
		Str("engine", name).
		Str("namespace", namespace).
		Str("state", currentState).
		Msg("Processing engine status")

	hasError := false
	var errorMessage string

	// Check engine state
	switch currentState {
	case "running":
		// Healthy state
	case "stopped", "error":
		hasError = true
		errorMessage = fmt.Sprintf("Engine in %s state", currentState)
	case "starting", "stopping":
		// Transitional states
		log.Debug().Str("engine", name).Str("state", currentState).Msg("Engine in transitional state")
	default:
		hasError = true
		errorMessage = fmt.Sprintf("Engine in unknown state: %s", currentState)
	}

	// Update state and send alerts
	updateLonghornEngineState(key, hasError, errorMessage, currentState, namespace)

	if hasError && shouldSendLonghornAlert("engine", key) {
		sendLonghornEngineAlert(name, namespace, currentState, errorMessage)
		markLonghornAlertSent("engine", key)
	} else if !hasError {
		checkLonghornEngineRecovery(key, name, namespace)
	}
}

// processLonghornNodeStatus processes the status of a Longhorn node
func processLonghornNodeStatus(name string, conditions []interface{}) {
	key := name

	log.Debug().
		Str("node", name).
		Int("conditions", len(conditions)).
		Msg("Processing node status")

	hasError := false
	var errorMessage string

	// Process node conditions
	for _, conditionInterface := range conditions {
		condition, ok := conditionInterface.(map[string]interface{})
		if !ok {
			continue
		}

		condType, _, _ := unstructured.NestedString(condition, "type")
		status, _, _ := unstructured.NestedString(condition, "status")
		reason, _, _ := unstructured.NestedString(condition, "reason")

		// Check for problematic conditions
		switch condType {
		case "Ready":
			if status != "True" {
				hasError = true
				errorMessage = fmt.Sprintf("Node not ready: %s", reason)
			}
		case "Schedulable":
			if status != "True" {
				hasError = true
				errorMessage = fmt.Sprintf("Node not schedulable: %s", reason)
			}
		}
	}

	// Update state and send alerts
	updateLonghornNodeState(key, hasError, errorMessage, "")

	if hasError && shouldSendLonghornAlert("node", key) {
		sendLonghornNodeAlert(name, errorMessage, conditions)
		markLonghornAlertSent("node", key)
	} else if !hasError {
		checkLonghornNodeRecovery(key, name)
	}
}

// processLonghornBackupStatus processes the status of a Longhorn backup
func processLonghornBackupStatus(name, namespace, state string) {
	key := fmt.Sprintf("%s/%s", namespace, name)

	log.Debug().
		Str("backup", name).
		Str("namespace", namespace).
		Str("state", state).
		Msg("Processing backup status")

	hasError := false
	var errorMessage string

	// Check backup state
	switch state {
	case "Completed":
		// Successful backup
	case "Error":
		hasError = true
		errorMessage = "Backup failed"
	case "InProgress", "Pending":
		// Normal transitional states
		log.Debug().Str("backup", name).Str("state", state).Msg("Backup in progress")
	default:
		hasError = true
		errorMessage = fmt.Sprintf("Backup in unknown state: %s", state)
	}

	// Update state and send alerts
	updateLonghornBackupState(key, hasError, errorMessage, state, namespace)

	if hasError && shouldSendLonghornAlert("backup", key) {
		sendLonghornBackupAlert(name, namespace, state, errorMessage)
		markLonghornAlertSent("backup", key)
	} else if !hasError && state == "Completed" {
		checkLonghornBackupRecovery(key, name, namespace)
	}
}

// State update functions
func updateLonghornVolumeState(key string, hasError bool, errorMessage, state, robustness string, capacity, actualSize int64, namespace string) {
	longhornVolumeStatesLock.Lock()
	defer longhornVolumeStatesLock.Unlock()

	now := time.Now()
	prevState, exists := longhornVolumeStates[key]

	newState := longhornUnitState{
		unitState: unitState{
			hasError:    hasError,
			lastSeen:    now,
			lastMessage: errorMessage,
		},
		resourceType: "volume",
		capacity:     capacity,
		usage:        actualSize,
		robustness:   robustness,
		namespace:    namespace,
	}

	if !exists {
		newState.firstError = now
		newState.alertSent = false
	} else {
		if hasError && !prevState.hasError {
			newState.firstError = now
			newState.alertSent = false
		} else if !hasError {
			newState.firstError = time.Time{}
			newState.alertSent = false
		} else {
			newState.firstError = prevState.firstError
			newState.alertSent = prevState.alertSent
		}
	}

	longhornVolumeStates[key] = newState
}

func updateLonghornReplicaState(key string, hasError bool, errorMessage, currentState, namespace string) {
	longhornReplicaStatesLock.Lock()
	defer longhornReplicaStatesLock.Unlock()

	now := time.Now()
	prevState, exists := longhornReplicaStates[key]

	newState := longhornUnitState{
		unitState: unitState{
			hasError:    hasError,
			lastSeen:    now,
			lastMessage: errorMessage,
		},
		resourceType: "replica",
		namespace:    namespace,
	}

	if !exists {
		newState.firstError = now
		newState.alertSent = false
	} else {
		if hasError && !prevState.hasError {
			newState.firstError = now
			newState.alertSent = false
		} else if !hasError {
			newState.firstError = time.Time{}
			newState.alertSent = false
		} else {
			newState.firstError = prevState.firstError
			newState.alertSent = prevState.alertSent
		}
	}

	longhornReplicaStates[key] = newState
}

func updateLonghornEngineState(key string, hasError bool, errorMessage, currentState, namespace string) {
	longhornEngineStatesLock.Lock()
	defer longhornEngineStatesLock.Unlock()

	now := time.Now()
	prevState, exists := longhornEngineStates[key]

	newState := longhornUnitState{
		unitState: unitState{
			hasError:    hasError,
			lastSeen:    now,
			lastMessage: errorMessage,
		},
		resourceType: "engine",
		namespace:    namespace,
	}

	if !exists {
		newState.firstError = now
		newState.alertSent = false
	} else {
		if hasError && !prevState.hasError {
			newState.firstError = now
			newState.alertSent = false
		} else if !hasError {
			newState.firstError = time.Time{}
			newState.alertSent = false
		} else {
			newState.firstError = prevState.firstError
			newState.alertSent = prevState.alertSent
		}
	}

	longhornEngineStates[key] = newState
}

func updateLonghornNodeState(key string, hasError bool, errorMessage, nodeName string) {
	longhornNodeStatesLock.Lock()
	defer longhornNodeStatesLock.Unlock()

	now := time.Now()
	prevState, exists := longhornNodeStates[key]

	newState := longhornUnitState{
		unitState: unitState{
			hasError:    hasError,
			lastSeen:    now,
			lastMessage: errorMessage,
		},
		resourceType: "node",
		node:         nodeName,
	}

	if !exists {
		newState.firstError = now
		newState.alertSent = false
	} else {
		if hasError && !prevState.hasError {
			newState.firstError = now
			newState.alertSent = false
		} else if !hasError {
			newState.firstError = time.Time{}
			newState.alertSent = false
		} else {
			newState.firstError = prevState.firstError
			newState.alertSent = prevState.alertSent
		}
	}

	longhornNodeStates[key] = newState
}

func updateLonghornBackupState(key string, hasError bool, errorMessage, state, namespace string) {
	longhornBackupStatesLock.Lock()
	defer longhornBackupStatesLock.Unlock()

	now := time.Now()
	prevState, exists := longhornBackupStates[key]

	newState := longhornUnitState{
		unitState: unitState{
			hasError:    hasError,
			lastSeen:    now,
			lastMessage: errorMessage,
		},
		resourceType: "backup",
		namespace:    namespace,
	}

	if !exists {
		newState.firstError = now
		newState.alertSent = false
	} else {
		if hasError && !prevState.hasError {
			newState.firstError = now
			newState.alertSent = false
		} else if !hasError {
			newState.firstError = time.Time{}
			newState.alertSent = false
		} else {
			newState.firstError = prevState.firstError
			newState.alertSent = prevState.alertSent
		}
	}

	longhornBackupStates[key] = newState
}
