package main

import (
	"fmt"
	"sync"
	"time"

	log "github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
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
