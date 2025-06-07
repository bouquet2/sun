package main

import (
	"fmt"

	log "github.com/rs/zerolog/log"
)

// sendLonghornVolumeAlert sends an alert for a Longhorn volume issue
func sendLonghornVolumeAlert(name, namespace, state, robustness string, capacity, actualSize int64, errorMessage, alertType string) {
	// Calculate usage percentage for display
	usagePercent := float64(0)
	if capacity > 0 && actualSize > 0 {
		usagePercent = float64(actualSize) / float64(capacity) * 100
	}

	// Format capacity and usage for display
	capacityGB := float64(capacity) / (1024 * 1024 * 1024)
	usageGB := float64(actualSize) / (1024 * 1024 * 1024)

	alert := Alert{
		Title:       fmt.Sprintf("Longhorn Volume Alert on %s", namespace),
		Description: fmt.Sprintf("Volume %s: %s", name, errorMessage),
		Fields: []struct {
			Name   string
			Value  string
			Inline bool
		}{
			{Name: "Volume", Value: name, Inline: true},
			{Name: "Namespace", Value: namespace, Inline: true},
			{Name: "State", Value: state, Inline: true},
			{Name: "Robustness", Value: robustness, Inline: true},
			{Name: "Alert Type", Value: alertType, Inline: true},
		},
	}

	// Add capacity information if available
	if capacity > 0 {
		alert.Fields = append(alert.Fields, struct {
			Name   string
			Value  string
			Inline bool
		}{
			Name:   "Capacity",
			Value:  fmt.Sprintf("%.2f GB", capacityGB),
			Inline: true,
		})
	}

	if actualSize > 0 {
		alert.Fields = append(alert.Fields, struct {
			Name   string
			Value  string
			Inline bool
		}{
			Name:   "Usage",
			Value:  fmt.Sprintf("%.2f GB (%.1f%%)", usageGB, usagePercent),
			Inline: true,
		})
	}

	sendWebhookMessage(alert)
	log.Error().
		Str("volume", name).
		Str("namespace", namespace).
		Str("state", state).
		Str("robustness", robustness).
		Str("alertType", alertType).
		Msg("Longhorn volume alert sent")
}

// sendLonghornReplicaAlert sends an alert for a Longhorn replica issue
func sendLonghornReplicaAlert(name, namespace, currentState, errorMessage string) {
	alert := Alert{
		Title:       fmt.Sprintf("Longhorn Replica Alert on %s", namespace),
		Description: fmt.Sprintf("Replica %s: %s", name, errorMessage),
		Fields: []struct {
			Name   string
			Value  string
			Inline bool
		}{
			{Name: "Replica", Value: name, Inline: true},
			{Name: "Namespace", Value: namespace, Inline: true},
			{Name: "State", Value: currentState, Inline: true},
		},
	}

	sendWebhookMessage(alert)
	log.Error().
		Str("replica", name).
		Str("namespace", namespace).
		Str("state", currentState).
		Msg("Longhorn replica alert sent")
}

// sendLonghornEngineAlert sends an alert for a Longhorn engine issue
func sendLonghornEngineAlert(name, namespace, currentState, errorMessage string) {
	alert := Alert{
		Title:       fmt.Sprintf("Longhorn Engine Alert on %s", namespace),
		Description: fmt.Sprintf("Engine %s: %s", name, errorMessage),
		Fields: []struct {
			Name   string
			Value  string
			Inline bool
		}{
			{Name: "Engine", Value: name, Inline: true},
			{Name: "Namespace", Value: namespace, Inline: true},
			{Name: "State", Value: currentState, Inline: true},
		},
	}

	sendWebhookMessage(alert)
	log.Error().
		Str("engine", name).
		Str("namespace", namespace).
		Str("state", currentState).
		Msg("Longhorn engine alert sent")
}

// sendLonghornNodeAlert sends an alert for a Longhorn node issue
func sendLonghornNodeAlert(name, errorMessage string, conditions []interface{}) {
	alert := Alert{
		Title:       fmt.Sprintf("Longhorn Node Alert"),
		Description: fmt.Sprintf("Node %s: %s", name, errorMessage),
		Fields: []struct {
			Name   string
			Value  string
			Inline bool
		}{
			{Name: "Node", Value: name, Inline: true},
			{Name: "Issue", Value: errorMessage, Inline: false},
		},
	}

	// Add condition details if available
	if len(conditions) > 0 {
		conditionDetails := ""
		for i, conditionInterface := range conditions {
			if condition, ok := conditionInterface.(map[string]interface{}); ok {
				if condType, ok := condition["type"].(string); ok {
					if status, ok := condition["status"].(string); ok {
						if i > 0 {
							conditionDetails += ", "
						}
						conditionDetails += fmt.Sprintf("%s=%s", condType, status)
					}
				}
			}
		}
		if conditionDetails != "" {
			alert.Fields = append(alert.Fields, struct {
				Name   string
				Value  string
				Inline bool
			}{
				Name:   "Conditions",
				Value:  conditionDetails,
				Inline: false,
			})
		}
	}

	sendWebhookMessage(alert)
	log.Error().
		Str("node", name).
		Str("error", errorMessage).
		Msg("Longhorn node alert sent")
}

// sendLonghornBackupAlert sends an alert for a Longhorn backup issue
func sendLonghornBackupAlert(name, namespace, state, errorMessage string) {
	alert := Alert{
		Title:       fmt.Sprintf("Longhorn Backup Alert on %s", namespace),
		Description: fmt.Sprintf("Backup %s: %s", name, errorMessage),
		Fields: []struct {
			Name   string
			Value  string
			Inline bool
		}{
			{Name: "Backup", Value: name, Inline: true},
			{Name: "Namespace", Value: namespace, Inline: true},
			{Name: "State", Value: state, Inline: true},
		},
	}

	sendWebhookMessage(alert)
	log.Error().
		Str("backup", name).
		Str("namespace", namespace).
		Str("state", state).
		Msg("Longhorn backup alert sent")
}

// Recovery functions

// checkLonghornVolumeRecovery checks if a volume has recovered and sends a recovery alert
func checkLonghornVolumeRecovery(key, name, namespace string) {
	longhornVolumeStatesLock.RLock()
	prevState, exists := longhornVolumeStates[key]
	longhornVolumeStatesLock.RUnlock()

	if exists && prevState.hasError && prevState.alertSent {
		alert := Alert{
			Title:       "Longhorn Volume Recovery",
			Description: fmt.Sprintf("Volume %s in namespace %s has recovered", name, namespace),
			Fields: []struct {
				Name   string
				Value  string
				Inline bool
			}{
				{Name: "Volume", Value: name, Inline: true},
				{Name: "Namespace", Value: namespace, Inline: true},
				{Name: "State", Value: "Healthy", Inline: true},
			},
		}
		sendWebhookMessage(alert)
		log.Info().
			Str("volume", name).
			Str("namespace", namespace).
			Msg("Longhorn volume has recovered")
	}
}

// checkLonghornReplicaRecovery checks if a replica has recovered and sends a recovery alert
func checkLonghornReplicaRecovery(key, name, namespace string) {
	longhornReplicaStatesLock.RLock()
	prevState, exists := longhornReplicaStates[key]
	longhornReplicaStatesLock.RUnlock()

	if exists && prevState.hasError && prevState.alertSent {
		alert := Alert{
			Title:       "Longhorn Replica Recovery",
			Description: fmt.Sprintf("Replica %s in namespace %s has recovered", name, namespace),
			Fields: []struct {
				Name   string
				Value  string
				Inline bool
			}{
				{Name: "Replica", Value: name, Inline: true},
				{Name: "Namespace", Value: namespace, Inline: true},
				{Name: "State", Value: "Running", Inline: true},
			},
		}
		sendWebhookMessage(alert)
		log.Info().
			Str("replica", name).
			Str("namespace", namespace).
			Msg("Longhorn replica has recovered")
	}
}

// checkLonghornEngineRecovery checks if an engine has recovered and sends a recovery alert
func checkLonghornEngineRecovery(key, name, namespace string) {
	longhornEngineStatesLock.RLock()
	prevState, exists := longhornEngineStates[key]
	longhornEngineStatesLock.RUnlock()

	if exists && prevState.hasError && prevState.alertSent {
		alert := Alert{
			Title:       "Longhorn Engine Recovery",
			Description: fmt.Sprintf("Engine %s in namespace %s has recovered", name, namespace),
			Fields: []struct {
				Name   string
				Value  string
				Inline bool
			}{
				{Name: "Engine", Value: name, Inline: true},
				{Name: "Namespace", Value: namespace, Inline: true},
				{Name: "State", Value: "Running", Inline: true},
			},
		}
		sendWebhookMessage(alert)
		log.Info().
			Str("engine", name).
			Str("namespace", namespace).
			Msg("Longhorn engine has recovered")
	}
}

// checkLonghornNodeRecovery checks if a node has recovered and sends a recovery alert
func checkLonghornNodeRecovery(key, name string) {
	longhornNodeStatesLock.RLock()
	prevState, exists := longhornNodeStates[key]
	longhornNodeStatesLock.RUnlock()

	if exists && prevState.hasError && prevState.alertSent {
		alert := Alert{
			Title:       "Longhorn Node Recovery",
			Description: fmt.Sprintf("Node %s has recovered", name),
			Fields: []struct {
				Name   string
				Value  string
				Inline bool
			}{
				{Name: "Node", Value: name, Inline: true},
				{Name: "State", Value: "Ready", Inline: true},
			},
		}
		sendWebhookMessage(alert)
		log.Info().
			Str("node", name).
			Msg("Longhorn node has recovered")
	}
}

// checkLonghornBackupRecovery checks if a backup has completed successfully after previous failures
func checkLonghornBackupRecovery(key, name, namespace string) {
	longhornBackupStatesLock.RLock()
	prevState, exists := longhornBackupStates[key]
	longhornBackupStatesLock.RUnlock()

	if exists && prevState.hasError && prevState.alertSent {
		alert := Alert{
			Title:       "Longhorn Backup Recovery",
			Description: fmt.Sprintf("Backup %s in namespace %s has completed successfully", name, namespace),
			Fields: []struct {
				Name   string
				Value  string
				Inline bool
			}{
				{Name: "Backup", Value: name, Inline: true},
				{Name: "Namespace", Value: namespace, Inline: true},
				{Name: "State", Value: "Completed", Inline: true},
			},
		}
		sendWebhookMessage(alert)
		log.Info().
			Str("backup", name).
			Str("namespace", namespace).
			Msg("Longhorn backup has completed successfully")
	}
}
