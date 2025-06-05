package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
)

var (
	podStates     = make(map[string]unitState)
	podStatesLock sync.RWMutex
)

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
			Err(err).
			Msg("Failed to fetch container logs")
		return "Failed to fetch logs, error: " + err.Error()
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
		podStates[podKey] = unitState{
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
		if shouldSendAlert("pod", podKey) {
			handleTerminatedContainer(pod, container)
			markAlertSent(podKey)
		}
	}

	if container.State.Waiting != nil {
		hasError = true
		errorMessage = fmt.Sprintf("Container %s is waiting", container.Name)
		if shouldSendAlert("pod", podKey) {
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
