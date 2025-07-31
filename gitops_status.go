package main

import (
	"fmt"
	"time"

	log "github.com/rs/zerolog/log"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// updateGitOpsState updates the state of a GitOps resource
func updateGitOpsState(key string, hasError bool, errorMessage, repositoryName, resourceKind, resourceName, namespace, mismatchType, expectedHash, actualHash string) {
	gitOpsStatesLock.Lock()
	defer gitOpsStatesLock.Unlock()

	now := time.Now()
	prevState, exists := gitOpsStates[key]

	newState := gitOpsState{
		unitState: unitState{
			hasError:    hasError,
			lastSeen:    now,
			lastMessage: errorMessage,
		},
		repositoryName: repositoryName,
		resourceKind:   resourceKind,
		resourceName:   resourceName,
		namespace:      namespace,
		mismatchType:   mismatchType,
		expectedHash:   expectedHash,
		actualHash:     actualHash,
	}

	// If this is a new error or the error has changed, reset the alert state
	if !exists || (!prevState.hasError && hasError) || (prevState.hasError && prevState.lastMessage != errorMessage) {
		newState.firstError = now
		newState.alertSent = false
	} else if exists && prevState.hasError {
		// Keep the original error time and alert state
		newState.firstError = prevState.firstError
		newState.alertSent = prevState.alertSent
	}

	gitOpsStates[key] = newState

	log.Debug().
		Str("key", key).
		Bool("hasError", hasError).
		Str("message", errorMessage).
		Msg("Updated GitOps state")
}

// shouldSendGitOpsAlert checks if we should send an alert for a GitOps resource
func shouldSendGitOpsAlert(key string) bool {
	gitOpsStatesLock.RLock()
	state, exists := gitOpsStates[key]
	gitOpsStatesLock.RUnlock()

	if !exists || !state.hasError || state.alertSent {
		return false
	}

	// Check if alerting is enabled globally and for this repository
	if !config.GitOps.AlertOnMismatch {
		return false
	}

	// Check repository-specific alert setting
	for _, repo := range config.GitOps.Repositories {
		if repo.Name == state.repositoryName && !repo.AlertOnMismatch {
			return false
		}
	}

	// If interval is 0, send alert immediately
	if config.Interval == 0 {
		return true
	}

	// Check if enough time has passed since the error was first seen
	intervalDuration := time.Duration(config.Interval) * time.Minute
	return time.Since(state.firstError) >= intervalDuration
}

// markGitOpsAlertSent marks an alert as sent for a GitOps resource
func markGitOpsAlertSent(key string) {
	gitOpsStatesLock.Lock()
	defer gitOpsStatesLock.Unlock()

	if state, exists := gitOpsStates[key]; exists {
		state.alertSent = true
		gitOpsStates[key] = state
	}
}

// sendGitOpsMismatchAlert sends an alert for a GitOps mismatch
func sendGitOpsMismatchAlert(repositoryName string, expected, actual *unstructured.Unstructured, mismatchType string) {
	var title, description string
	var resourceName, resourceKind, namespace string

	if expected != nil {
		resourceName = expected.GetName()
		resourceKind = expected.GetKind()
		namespace = expected.GetNamespace()
	} else if actual != nil {
		resourceName = actual.GetName()
		resourceKind = actual.GetKind()
		namespace = actual.GetNamespace()
	}

	switch mismatchType {
	case "missing":
		title = fmt.Sprintf("GitOps Alert: Missing Resource in %s", repositoryName)
		description = fmt.Sprintf("Resource %s/%s is defined in Git but missing from cluster", resourceKind, resourceName)
	case "different":
		title = fmt.Sprintf("GitOps Alert: Resource Drift in %s", repositoryName)
		description = fmt.Sprintf("Resource %s/%s differs between Git and cluster", resourceKind, resourceName)
	case "extra":
		title = fmt.Sprintf("GitOps Alert: Extra Resource in %s", repositoryName)
		description = fmt.Sprintf("Resource %s/%s exists in cluster but not in Git", resourceKind, resourceName)
	default:
		title = fmt.Sprintf("GitOps Alert: Unknown Issue in %s", repositoryName)
		description = fmt.Sprintf("Unknown mismatch type %s for resource %s/%s", mismatchType, resourceKind, resourceName)
	}

	alert := Alert{
		Title:       title,
		Description: description,
		Fields: []struct {
			Name   string
			Value  string
			Inline bool
		}{
			{Name: "Repository", Value: repositoryName, Inline: true},
			{Name: "Resource Kind", Value: resourceKind, Inline: true},
			{Name: "Resource Name", Value: resourceName, Inline: true},
		},
	}

	if namespace != "" {
		alert.Fields = append(alert.Fields, struct {
			Name   string
			Value  string
			Inline bool
		}{Name: "Namespace", Value: namespace, Inline: true})
	}

	alert.Fields = append(alert.Fields, struct {
		Name   string
		Value  string
		Inline bool
	}{Name: "Mismatch Type", Value: mismatchType, Inline: true})

	// Add additional context based on mismatch type
	switch mismatchType {
	case "missing":
		alert.Fields = append(alert.Fields, struct {
			Name   string
			Value  string
			Inline bool
		}{Name: "Action Required", Value: "Apply the resource to the cluster or remove from Git", Inline: false})
	case "different":
		alert.Fields = append(alert.Fields, struct {
			Name   string
			Value  string
			Inline bool
		}{Name: "Action Required", Value: "Review differences and either update Git or apply changes to cluster", Inline: false})
	case "extra":
		alert.Fields = append(alert.Fields, struct {
			Name   string
			Value  string
			Inline bool
		}{Name: "Action Required", Value: "Remove resource from cluster or add to Git repository", Inline: false})
	}

	sendWebhookMessage(alert)
	log.Error().
		Str("repository", repositoryName).
		Str("kind", resourceKind).
		Str("name", resourceName).
		Str("namespace", namespace).
		Str("mismatchType", mismatchType).
		Msg("GitOps mismatch alert sent")
}

// checkGitOpsRecovery checks if a GitOps resource has recovered and sends a recovery alert
func checkGitOpsRecovery(key, repositoryName, resourceKind, resourceName, namespace string) {
	gitOpsStatesLock.RLock()
	prevState, exists := gitOpsStates[key]
	gitOpsStatesLock.RUnlock()

	if exists && prevState.hasError && prevState.alertSent {
		alert := Alert{
			Title:       fmt.Sprintf("GitOps Recovery: %s", repositoryName),
			Description: fmt.Sprintf("Resource %s/%s is now in sync between Git and cluster", resourceKind, resourceName),
			Fields: []struct {
				Name   string
				Value  string
				Inline bool
			}{
				{Name: "Repository", Value: repositoryName, Inline: true},
				{Name: "Resource Kind", Value: resourceKind, Inline: true},
				{Name: "Resource Name", Value: resourceName, Inline: true},
			},
		}

		if namespace != "" {
			alert.Fields = append(alert.Fields, struct {
				Name   string
				Value  string
				Inline bool
			}{Name: "Namespace", Value: namespace, Inline: true})
		}

		alert.Fields = append(alert.Fields, struct {
			Name   string
			Value  string
			Inline bool
		}{Name: "Status", Value: "✅ In Sync", Inline: true})

		sendWebhookMessage(alert)
		log.Info().
			Str("repository", repositoryName).
			Str("kind", resourceKind).
			Str("name", resourceName).
			Str("namespace", namespace).
			Msg("GitOps resource has recovered")
	}
}
