package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	log "github.com/rs/zerolog/log"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// Longhorn CRD GroupVersionResources
var (
	longhornVolumes = schema.GroupVersionResource{
		Group:    "longhorn.io",
		Version:  "v1beta2",
		Resource: "volumes",
	}
	longhornReplicas = schema.GroupVersionResource{
		Group:    "longhorn.io",
		Version:  "v1beta2",
		Resource: "replicas",
	}
	longhornEngines = schema.GroupVersionResource{
		Group:    "longhorn.io",
		Version:  "v1beta2",
		Resource: "engines",
	}
	longhornNodes = schema.GroupVersionResource{
		Group:    "longhorn.io",
		Version:  "v1beta2",
		Resource: "nodes",
	}
	longhornBackups = schema.GroupVersionResource{
		Group:    "longhorn.io",
		Version:  "v1beta2",
		Resource: "backups",
	}
)

// setupLonghornInformers sets up informers for Longhorn CRDs
func setupLonghornInformers(ctx context.Context) error {
	if !config.Longhorn.Enabled {
		log.Info().Msg("Longhorn monitoring is disabled")
		return nil
	}

	// Set default namespace if not specified
	longhornNamespace := config.Longhorn.Namespace
	if longhornNamespace == "" {
		longhornNamespace = "longhorn-system"
	}

	log.Info().Str("namespace", longhornNamespace).Msg("Setting up Longhorn monitoring")

	// Create dynamic informer factory for Longhorn namespace
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicClient,
		0,
		longhornNamespace,
		nil,
	)

	// Setup Volume informer
	if config.Longhorn.Monitor.Volumes {
		volumeInformer := factory.ForResource(longhornVolumes).Informer()
		volumeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    handleLonghornVolume,
			UpdateFunc: func(_, obj interface{}) { handleLonghornVolume(obj) },
			DeleteFunc: handleLonghornVolumeDelete,
		})
		log.Debug().Msg("Longhorn Volume informer configured")
	}

	// Setup Replica informer
	if config.Longhorn.Monitor.Replicas {
		replicaInformer := factory.ForResource(longhornReplicas).Informer()
		replicaInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    handleLonghornReplica,
			UpdateFunc: func(_, obj interface{}) { handleLonghornReplica(obj) },
			DeleteFunc: handleLonghornReplicaDelete,
		})
		log.Debug().Msg("Longhorn Replica informer configured")
	}

	// Setup Engine informer
	if config.Longhorn.Monitor.Engines {
		engineInformer := factory.ForResource(longhornEngines).Informer()
		engineInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    handleLonghornEngine,
			UpdateFunc: func(_, obj interface{}) { handleLonghornEngine(obj) },
			DeleteFunc: handleLonghornEngineDelete,
		})
		log.Debug().Msg("Longhorn Engine informer configured")
	}

	// Setup Node informer
	if config.Longhorn.Monitor.Nodes {
		nodeInformer := factory.ForResource(longhornNodes).Informer()
		nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    handleLonghornNode,
			UpdateFunc: func(_, obj interface{}) { handleLonghornNode(obj) },
			DeleteFunc: handleLonghornNodeDelete,
		})
		log.Debug().Msg("Longhorn Node informer configured")
	}

	// Setup Backup informer
	if config.Longhorn.Monitor.Backups {
		backupInformer := factory.ForResource(longhornBackups).Informer()
		backupInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    handleLonghornBackup,
			UpdateFunc: func(_, obj interface{}) { handleLonghornBackup(obj) },
			DeleteFunc: handleLonghornBackupDelete,
		})
		log.Debug().Msg("Longhorn Backup informer configured")
	}

	// Start informers
	go factory.Start(ctx.Done())

	log.Info().Msg("Longhorn informers started")
	return nil
}

// Volume handlers
func handleLonghornVolume(obj interface{}) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Error().Msg("Received non-unstructured object in Longhorn volume informer")
		return
	}

	name := unstructuredObj.GetName()
	namespace := unstructuredObj.GetNamespace()

	log.Debug().
		Str("volume", name).
		Str("namespace", namespace).
		Msg("Processing Longhorn volume")

	// Extract volume status fields
	status, found, err := unstructured.NestedMap(unstructuredObj.Object, "status")
	if err != nil || !found {
		log.Debug().Str("volume", name).Msg("No status found for volume")
		return
	}

	// Get volume state and robustness
	state, _, _ := unstructured.NestedString(status, "state")
	robustness, _, _ := unstructured.NestedString(status, "robustness")

	// Get capacity and actual size
	spec, found, err := unstructured.NestedMap(unstructuredObj.Object, "spec")
	if err != nil || !found {
		log.Debug().Str("volume", name).Msg("No spec found for volume")
		return
	}

	sizeStr, _, _ := unstructured.NestedString(spec, "size")
	capacity := parseSize(sizeStr)

	actualSize, _, _ := unstructured.NestedInt64(status, "actualSize")

	processLonghornVolumeStatus(name, namespace, state, robustness, capacity, actualSize)
}

func handleLonghornVolumeDelete(obj interface{}) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}

	key := fmt.Sprintf("%s/%s", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
	longhornVolumeStatesLock.Lock()
	delete(longhornVolumeStates, key)
	longhornVolumeStatesLock.Unlock()
}

// Replica handlers
func handleLonghornReplica(obj interface{}) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Error().Msg("Received non-unstructured object in Longhorn replica informer")
		return
	}

	name := unstructuredObj.GetName()
	namespace := unstructuredObj.GetNamespace()

	log.Debug().
		Str("replica", name).
		Str("namespace", namespace).
		Msg("Processing Longhorn replica")

	// Extract replica status
	status, found, err := unstructured.NestedMap(unstructuredObj.Object, "status")
	if err != nil || !found {
		return
	}

	currentState, _, _ := unstructured.NestedString(status, "currentState")

	processLonghornReplicaStatus(name, namespace, currentState)
}

func handleLonghornReplicaDelete(obj interface{}) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}

	key := fmt.Sprintf("%s/%s", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
	longhornReplicaStatesLock.Lock()
	delete(longhornReplicaStates, key)
	longhornReplicaStatesLock.Unlock()
}

// Engine handlers
func handleLonghornEngine(obj interface{}) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Error().Msg("Received non-unstructured object in Longhorn engine informer")
		return
	}

	name := unstructuredObj.GetName()
	namespace := unstructuredObj.GetNamespace()

	log.Debug().
		Str("engine", name).
		Str("namespace", namespace).
		Msg("Processing Longhorn engine")

	// Extract engine status
	status, found, err := unstructured.NestedMap(unstructuredObj.Object, "status")
	if err != nil || !found {
		return
	}

	currentState, _, _ := unstructured.NestedString(status, "currentState")

	processLonghornEngineStatus(name, namespace, currentState)
}

func handleLonghornEngineDelete(obj interface{}) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}

	key := fmt.Sprintf("%s/%s", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
	longhornEngineStatesLock.Lock()
	delete(longhornEngineStates, key)
	longhornEngineStatesLock.Unlock()
}

// Node handlers
func handleLonghornNode(obj interface{}) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Error().Msg("Received non-unstructured object in Longhorn node informer")
		return
	}

	name := unstructuredObj.GetName()

	log.Debug().
		Str("node", name).
		Msg("Processing Longhorn node")

	// Extract node status
	status, found, err := unstructured.NestedMap(unstructuredObj.Object, "status")
	if err != nil || !found {
		return
	}

	// Check node conditions
	conditions, found, err := unstructured.NestedSlice(status, "conditions")
	if err != nil || !found {
		return
	}

	processLonghornNodeStatus(name, conditions)
}

func handleLonghornNodeDelete(obj interface{}) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}

	key := unstructuredObj.GetName()
	longhornNodeStatesLock.Lock()
	delete(longhornNodeStates, key)
	longhornNodeStatesLock.Unlock()
}

// Backup handlers
func handleLonghornBackup(obj interface{}) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Error().Msg("Received non-unstructured object in Longhorn backup informer")
		return
	}

	name := unstructuredObj.GetName()
	namespace := unstructuredObj.GetNamespace()

	log.Debug().
		Str("backup", name).
		Str("namespace", namespace).
		Msg("Processing Longhorn backup")

	// Extract backup status
	status, found, err := unstructured.NestedMap(unstructuredObj.Object, "status")
	if err != nil || !found {
		return
	}

	state, _, _ := unstructured.NestedString(status, "state")

	processLonghornBackupStatus(name, namespace, state)
}

func handleLonghornBackupDelete(obj interface{}) {
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}

	key := fmt.Sprintf("%s/%s", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
	longhornBackupStatesLock.Lock()
	delete(longhornBackupStates, key)
	longhornBackupStatesLock.Unlock()
}

// Helper function to parse size strings
func parseSize(sizeStr string) int64 {
	if sizeStr == "" {
		return 0
	}

	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		log.Debug().Str("size", sizeStr).Msg("Failed to parse size")
		return 0
	}

	return size
}

// shouldSendLonghornAlert checks if we should send an alert for a Longhorn resource
func shouldSendLonghornAlert(resourceType string, key string) bool {
	var state longhornUnitState
	var exists bool

	switch resourceType {
	case "volume":
		longhornVolumeStatesLock.RLock()
		state, exists = longhornVolumeStates[key]
		longhornVolumeStatesLock.RUnlock()
	case "replica":
		longhornReplicaStatesLock.RLock()
		state, exists = longhornReplicaStates[key]
		longhornReplicaStatesLock.RUnlock()
	case "engine":
		longhornEngineStatesLock.RLock()
		state, exists = longhornEngineStates[key]
		longhornEngineStatesLock.RUnlock()
	case "node":
		longhornNodeStatesLock.RLock()
		state, exists = longhornNodeStates[key]
		longhornNodeStatesLock.RUnlock()
	case "backup":
		longhornBackupStatesLock.RLock()
		state, exists = longhornBackupStates[key]
		longhornBackupStatesLock.RUnlock()
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

// markLonghornAlertSent marks an alert as sent for a Longhorn resource
func markLonghornAlertSent(resourceType string, key string) {
	switch resourceType {
	case "volume":
		longhornVolumeStatesLock.Lock()
		defer longhornVolumeStatesLock.Unlock()
		if state, exists := longhornVolumeStates[key]; exists {
			state.alertSent = true
			longhornVolumeStates[key] = state
		}
	case "replica":
		longhornReplicaStatesLock.Lock()
		defer longhornReplicaStatesLock.Unlock()
		if state, exists := longhornReplicaStates[key]; exists {
			state.alertSent = true
			longhornReplicaStates[key] = state
		}
	case "engine":
		longhornEngineStatesLock.Lock()
		defer longhornEngineStatesLock.Unlock()
		if state, exists := longhornEngineStates[key]; exists {
			state.alertSent = true
			longhornEngineStates[key] = state
		}
	case "node":
		longhornNodeStatesLock.Lock()
		defer longhornNodeStatesLock.Unlock()
		if state, exists := longhornNodeStates[key]; exists {
			state.alertSent = true
			longhornNodeStates[key] = state
		}
	case "backup":
		longhornBackupStatesLock.Lock()
		defer longhornBackupStatesLock.Unlock()
		if state, exists := longhornBackupStates[key]; exists {
			state.alertSent = true
			longhornBackupStates[key] = state
		}
	}
}
