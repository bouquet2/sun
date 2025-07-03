package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	nodeStates     = make(map[string]unitState)
	nodeStatesLock sync.RWMutex
)

func markNodeAlertSent(nodeKey string) {
	nodeStatesLock.Lock()
	defer nodeStatesLock.Unlock()

	if state, exists := nodeStates[nodeKey]; exists {
		state.alertSent = true
		nodeStates[nodeKey] = state
	}
}

func getNodeCondition(node *corev1.Node, condType corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range node.Status.Conditions {
		c := &node.Status.Conditions[i]
		if c.Type == condType {
			return c
		}
	}
	return nil
}

func handleNodeAlert(node *corev1.Node, cond *corev1.NodeCondition) {
	alert := Alert{
		Title:       fmt.Sprintf("Node %s: %s", node.Name, cond.Type),
		Description: fmt.Sprintf("Node %s has condition %s = %s", node.Name, cond.Type, cond.Status),
		Fields: []struct {
			Name   string
			Value  string
			Inline bool
		}{
			{Name: "Node", Value: node.Name, Inline: true},
			{Name: "Condition", Value: string(cond.Type), Inline: true},
			{Name: "Status", Value: string(cond.Status), Inline: true},
			{Name: "Reason", Value: cond.Reason, Inline: false},
			{Name: "Message", Value: cond.Message, Inline: false},
		},
	}
	sendWebhookMessage(alert)
	log.Error().
		Str("node", node.Name).
		Str("condition", string(cond.Type)).
		Str("status", string(cond.Status)).
		Str("reason", cond.Reason).
		Msg("Node condition alert sent")
}

func handleNodeRecovery(node *corev1.Node) {
	alert := Alert{
		Title:       fmt.Sprintf("Node %s Recovery", node.Name),
		Description: fmt.Sprintf("Node %s has recovered all conditions", node.Name),
		Fields: []struct {
			Name   string
			Value  string
			Inline bool
		}{
			{Name: "Node", Value: node.Name, Inline: true},
			{Name: "State", Value: "Ready", Inline: true},
		},
	}
	sendWebhookMessage(alert)
	log.Info().
		Str("node", node.Name).
		Msg("Node has recovered")
}

func updateNodeState(node *corev1.Node, hasError bool, errorMessage string) {
	nodeKey := node.Name
	nodeStatesLock.Lock()
	defer nodeStatesLock.Unlock()

	now := time.Now()
	prevState, exists := nodeStates[nodeKey]

	if !exists {
		nodeStates[nodeKey] = unitState{
			hasError:    hasError,
			lastSeen:    now,
			lastMessage: errorMessage,
			firstError:  now,
			alertSent:   false,
		}
		return
	}

	if hasError && !prevState.hasError {
		prevState.firstError = now
		prevState.alertSent = false
	} else if !hasError {
		prevState.firstError = time.Time{}
		prevState.alertSent = false
	}
	prevState.hasError = hasError
	prevState.lastSeen = now
	prevState.lastMessage = errorMessage
	nodeStates[nodeKey] = prevState
}

func processNodeStatus(node *corev1.Node) (bool, string) {
	var hasError bool
	var errorMessage string
	nodeKey := node.Name

	readyCond := getNodeCondition(node, corev1.NodeReady)
	memoryCond := getNodeCondition(node, corev1.NodeMemoryPressure)
	diskCond := getNodeCondition(node, corev1.NodeDiskPressure)

	// Unhealthy conditions for alerting
	if readyCond != nil && (readyCond.Status == corev1.ConditionFalse || readyCond.Status == corev1.ConditionUnknown) {
		hasError = true
		errorMessage = "Node not Ready"
		if shouldSendAlert("node", nodeKey) {
			handleNodeAlert(node, readyCond)
			markNodeAlertSent(nodeKey)
		}
	}
	if memoryCond != nil && memoryCond.Status == corev1.ConditionTrue {
		hasError = true
		errorMessage = "Node under MemoryPressure"
		if shouldSendAlert("node", nodeKey) {
			handleNodeAlert(node, memoryCond)
			markNodeAlertSent(nodeKey)
		}
	}
	if diskCond != nil && diskCond.Status == corev1.ConditionTrue {
		hasError = true
		errorMessage = "Node under DiskPressure"
		if shouldSendAlert("node", nodeKey) {
			handleNodeAlert(node, diskCond)
			markNodeAlertSent(nodeKey)
		}
	}

	// Success path (node healthy)
	if readyCond != nil && readyCond.Status == corev1.ConditionTrue &&
		(memoryCond == nil || memoryCond.Status == corev1.ConditionFalse) &&
		(diskCond == nil || diskCond.Status == corev1.ConditionFalse) {
		nodeStatesLock.RLock()
		state, exists := nodeStates[nodeKey]
		nodeStatesLock.RUnlock()
		if exists && state.hasError && state.alertSent {
			handleNodeRecovery(node)
		}
	}

	return hasError, errorMessage
}

// calculateNodeResourceUsage calculates the CPU usage of a node based on pod requests
func calculateNodeResourceUsage(nodeName string) (cpuCapacity, cpuRequests int64, err error) {
	// Get node information to find capacity
	node, err := client.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get node %s: %v", nodeName, err)
	}

	// Get node CPU capacity
	cpuCapacityQuantity := node.Status.Allocatable[corev1.ResourceCPU]
	cpuCapacity = cpuCapacityQuantity.MilliValue()

	// Get all pods on this node
	pods, err := client.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to list pods on node %s: %v", nodeName, err)
	}

	// Sum up CPU requests from all pods on the node
	for _, pod := range pods.Items {
		// Skip pods that are not running or pending
		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
			continue
		}

		for _, container := range pod.Spec.Containers {
			if container.Resources.Requests != nil {
				if cpuRequest, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
					cpuRequests += cpuRequest.MilliValue()
				}
			}
		}
	}

	return cpuCapacity, cpuRequests, nil
}

// updateNodeResourceState updates the state of node resource monitoring
func updateNodeResourceState(nodeName string, hasError bool, errorMessage string, cpuCapacity, cpuRequests int64, cpuUsagePercent float64) {
	nodeResourceStatesLock.Lock()
	defer nodeResourceStatesLock.Unlock()

	now := time.Now()
	prevState, exists := nodeResourceStates[nodeName]

	newState := nodeResourceState{
		unitState: unitState{
			hasError:    hasError,
			lastSeen:    now,
			lastMessage: errorMessage,
		},
		cpuCapacity:     cpuCapacity,
		cpuRequests:     cpuRequests,
		cpuUsagePercent: cpuUsagePercent,
		nodeName:        nodeName,
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

	nodeResourceStates[nodeName] = newState
}

// markNodeResourceAlertSent marks a node resource alert as sent
func markNodeResourceAlertSent(nodeName string) {
	nodeResourceStatesLock.Lock()
	defer nodeResourceStatesLock.Unlock()

	if state, exists := nodeResourceStates[nodeName]; exists {
		state.alertSent = true
		nodeResourceStates[nodeName] = state
	}
}

// processNodeResourceUsage processes node resource usage and sends alerts if necessary
func processNodeResourceUsage(nodeName string) {
	if !config.NodeMonitoring.Enabled {
		return
	}

	cpuCapacity, cpuRequests, err := calculateNodeResourceUsage(nodeName)
	if err != nil {
		log.Error().Err(err).Str("node", nodeName).Msg("Failed to calculate node resource usage")
		return
	}

	// Calculate CPU usage percentage
	var cpuUsagePercent float64
	if cpuCapacity > 0 {
		cpuUsagePercent = float64(cpuRequests) / float64(cpuCapacity) * 100
	}

	log.Debug().
		Str("node", nodeName).
		Float64("cpu_usage_percent", cpuUsagePercent).
		Int64("cpu_capacity_millicores", cpuCapacity).
		Int64("cpu_requests_millicores", cpuRequests).
		Msg("Node resource usage calculated")

	hasError := false
	var errorMessage string

	// Check CPU usage
	if cpuUsagePercent > config.NodeMonitoring.CPUThresholdPercent {
		hasError = true
		errorMessage = fmt.Sprintf("CPU usage %.1f%% exceeds threshold %.1f%%", cpuUsagePercent, config.NodeMonitoring.CPUThresholdPercent)

		if shouldSendAlert("node_resource", nodeName) {
			alert := Alert{
				Title:       fmt.Sprintf("Node %s CPU Alert", nodeName),
				Description: fmt.Sprintf("Node %s CPU usage is above threshold", nodeName),
				Fields: []struct {
					Name   string
					Value  string
					Inline bool
				}{
					{Name: "Node", Value: nodeName, Inline: true},
					{Name: "CPU Usage", Value: fmt.Sprintf("%.1f%%", cpuUsagePercent), Inline: true},
					{Name: "Threshold", Value: fmt.Sprintf("%.1f%%", config.NodeMonitoring.CPUThresholdPercent), Inline: true},
				},
			}
			sendWebhookMessage(alert)
			markNodeResourceAlertSent(nodeName)
			log.Error().
				Str("node", nodeName).
				Float64("cpu_usage_percent", cpuUsagePercent).
				Float64("threshold", config.NodeMonitoring.CPUThresholdPercent).
				Msg("Node CPU usage alert sent")
		}
	}

	// Check for recovery
	if !hasError {
		nodeResourceStatesLock.RLock()
		prevState, exists := nodeResourceStates[nodeName]
		nodeResourceStatesLock.RUnlock()

		if exists && prevState.hasError && prevState.alertSent {
			if prevState.cpuUsagePercent > config.NodeMonitoring.CPUThresholdPercent {
				alert := Alert{
					Title:       fmt.Sprintf("Node %s CPU Recovery", nodeName),
					Description: fmt.Sprintf("Node %s CPU usage has returned to normal levels", nodeName),
					Fields: []struct {
						Name   string
						Value  string
						Inline bool
					}{
						{Name: "Node", Value: nodeName, Inline: true},
						{Name: "Current CPU Usage", Value: fmt.Sprintf("%.1f%%", cpuUsagePercent), Inline: true},
					},
				}
				sendWebhookMessage(alert)
				log.Info().
					Str("node", nodeName).
					Float64("cpu_usage_percent", cpuUsagePercent).
					Msg("Node CPU usage recovery alert sent")
			}
		}
	}

	// Update state
	updateNodeResourceState(nodeName, hasError, errorMessage, cpuCapacity, cpuRequests, cpuUsagePercent)
}
