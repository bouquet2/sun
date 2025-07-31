package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	log "github.com/rs/zerolog/log"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Cache for discovered GVR mappings
var (
	gvrCache     = make(map[string]schema.GroupVersionResource)
	gvrCacheLock sync.RWMutex
	gvrCacheInit sync.Once
)

// compareManifests compares generated manifests with live cluster resources
func compareManifests(repoState *gitOpsRepositoryState) error {
	log.Debug().Str("repository", repoState.name).Msg("Comparing manifests with cluster state")

	// Generate manifests using Kustomize
	manifests, err := generateKustomizeManifests(repoState)
	if err != nil {
		return fmt.Errorf("failed to generate manifests for repository %s: %w", repoState.name, err)
	}

	log.Debug().
		Str("repository", repoState.name).
		Int("manifests", len(manifests)).
		Msg("Generated manifests from repository")

	// Compare each manifest with cluster state
	for _, manifest := range manifests {
		if err := compareManifestWithCluster(repoState, manifest); err != nil {
			log.Error().
				Err(err).
				Str("repository", repoState.name).
				Str("resource", fmt.Sprintf("%s/%s", manifest.GetKind(), manifest.GetName())).
				Msg("Failed to compare manifest with cluster")
		}
	}

	return nil
}

// compareManifestWithCluster compares a single manifest with its cluster counterpart
func compareManifestWithCluster(repoState *gitOpsRepositoryState, manifest *unstructured.Unstructured) error {
	kind := manifest.GetKind()
	name := manifest.GetName()
	namespace := manifest.GetNamespace()

	log.Debug().
		Str("repository", repoState.name).
		Str("kind", kind).
		Str("name", name).
		Str("namespace", namespace).
		Msg("Comparing manifest with cluster")

	// Get the GroupVersionResource for this resource
	gvr, err := getGVRForKind(kind)
	if err != nil {
		return fmt.Errorf("failed to get GVR for kind %s: %w", kind, err)
	}

	// Get the resource from the cluster
	var clusterResource *unstructured.Unstructured
	if namespace != "" {
		// Namespaced resource
		clusterResource, err = dynamicClient.Resource(gvr).Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	} else {
		// Cluster-scoped resource
		clusterResource, err = dynamicClient.Resource(gvr).Get(context.TODO(), name, metav1.GetOptions{})
	}

	if err != nil {
		if errors.IsNotFound(err) {
			// Resource is missing from cluster
			return processGitOpsMismatch(repoState, manifest, nil, "missing")
		}
		return fmt.Errorf("failed to get resource %s/%s from cluster: %w", kind, name, err)
	}

	// Compare the resources
	if resourcesAreDifferent(manifest, clusterResource) {
		return processGitOpsMismatch(repoState, manifest, clusterResource, "different")
	}

	// Resources match - clear any previous error state
	return processGitOpsMatch(repoState, manifest)
}

// resourcesAreDifferent compares two unstructured resources using server-side apply dry-run
func resourcesAreDifferent(expected, actual *unstructured.Unstructured) bool {
	// Get the GroupVersionResource for this resource
	gvr, err := getGVRForKind(expected.GetKind())
	if err != nil {
		log.Error().Err(err).Str("kind", expected.GetKind()).Msg("Failed to get GVR for resource comparison")
		return false // If we can't get GVR, assume no difference to avoid false positives
	}

	// Perform server-side apply dry-run to see if there would be changes
	// This is exactly what kubectl diff does internally
	var result *unstructured.Unstructured

	if expected.GetNamespace() != "" {
		// Namespaced resource
		result, err = dynamicClient.Resource(gvr).Namespace(expected.GetNamespace()).
			Apply(context.TODO(), expected.GetName(), expected, metav1.ApplyOptions{
				DryRun:       []string{metav1.DryRunAll},
				FieldManager: "sun-gitops",
				Force:        true,
			})
	} else {
		// Cluster-scoped resource
		result, err = dynamicClient.Resource(gvr).
			Apply(context.TODO(), expected.GetName(), expected, metav1.ApplyOptions{
				DryRun:       []string{metav1.DryRunAll},
				FieldManager: "sun-gitops",
				Force:        true,
			})
	}

	if err != nil {
		log.Error().
			Err(err).
			Str("kind", expected.GetKind()).
			Str("name", expected.GetName()).
			Str("namespace", expected.GetNamespace()).
			Msg("Failed to perform server-side apply dry-run")
		return false // If dry-run fails, assume no difference to avoid false positives
	}

	// Compare the spec and metadata of the dry-run result with the actual resource
	// The dry-run result shows what the resource would look like after applying the expected manifest
	// If it's different from the actual resource, there's drift
	different := !resourcesEqual(result, actual)

	if different {
		log.Debug().
			Str("kind", expected.GetKind()).
			Str("name", expected.GetName()).
			Str("namespace", expected.GetNamespace()).
			Msg("Server-side apply dry-run detected differences")

		// Log the differences for debugging
		if log.Debug().Enabled() {
			resultJSON, _ := json.MarshalIndent(result.Object, "", "  ")
			actualJSON, _ := json.MarshalIndent(actual.Object, "", "  ")
			log.Debug().
				Str("kind", expected.GetKind()).
				Str("name", expected.GetName()).
				Str("dryRunResult", string(resultJSON)).
				Str("actualResource", string(actualJSON)).
				Msg("Dry-run vs actual resource comparison")
		}
	}

	return different
}

// resourcesEqual compares the meaningful parts of two resources
func resourcesEqual(dryRunResult, actual *unstructured.Unstructured) bool {
	if dryRunResult == nil || actual == nil {
		return dryRunResult == actual
	}

	// Compare the spec sections - this is where the actual configuration lives
	dryRunSpec, dryRunSpecExists, _ := unstructured.NestedMap(dryRunResult.Object, "spec")
	actualSpec, actualSpecExists, _ := unstructured.NestedMap(actual.Object, "spec")

	if dryRunSpecExists != actualSpecExists {
		return false
	}

	if dryRunSpecExists {
		dryRunSpecJSON, _ := json.Marshal(dryRunSpec)
		actualSpecJSON, _ := json.Marshal(actualSpec)
		if string(dryRunSpecJSON) != string(actualSpecJSON) {
			return false
		}
	}

	// Compare relevant metadata (labels and annotations that aren't system-managed)
	dryRunMeta, dryRunMetaExists, _ := unstructured.NestedMap(dryRunResult.Object, "metadata")
	actualMeta, actualMetaExists, _ := unstructured.NestedMap(actual.Object, "metadata")

	if dryRunMetaExists && actualMetaExists {
		// Compare labels (excluding system-managed ones)
		dryRunLabels, _, _ := unstructured.NestedStringMap(dryRunMeta, "labels")
		actualLabels, _, _ := unstructured.NestedStringMap(actualMeta, "labels")

		// Remove system-managed labels for comparison
		cleanLabels := func(labels map[string]string) map[string]string {
			cleaned := make(map[string]string)
			for k, v := range labels {
				// Skip system-managed labels
				if k == "app.kubernetes.io/managed-by" ||
					k == "helm.sh/chart" ||
					k == "app.kubernetes.io/instance" ||
					k == "app.kubernetes.io/version" {
					continue
				}
				cleaned[k] = v
			}
			return cleaned
		}

		cleanedDryRunLabels := cleanLabels(dryRunLabels)
		cleanedActualLabels := cleanLabels(actualLabels)

		dryRunLabelsJSON, _ := json.Marshal(cleanedDryRunLabels)
		actualLabelsJSON, _ := json.Marshal(cleanedActualLabels)
		if string(dryRunLabelsJSON) != string(actualLabelsJSON) {
			return false
		}

		// Compare annotations (excluding system-managed ones)
		dryRunAnnotations, _, _ := unstructured.NestedStringMap(dryRunMeta, "annotations")
		actualAnnotations, _, _ := unstructured.NestedStringMap(actualMeta, "annotations")

		// Remove system-managed annotations for comparison
		cleanAnnotations := func(annotations map[string]string) map[string]string {
			cleaned := make(map[string]string)
			for k, v := range annotations {
				// Skip system-managed annotations
				if k == "kubectl.kubernetes.io/last-applied-configuration" ||
					k == "deployment.kubernetes.io/revision" ||
					k == "meta.helm.sh/release-name" ||
					k == "meta.helm.sh/release-namespace" {
					continue
				}
				cleaned[k] = v
			}
			return cleaned
		}

		cleanedDryRunAnnotations := cleanAnnotations(dryRunAnnotations)
		cleanedActualAnnotations := cleanAnnotations(actualAnnotations)

		dryRunAnnotationsJSON, _ := json.Marshal(cleanedDryRunAnnotations)
		actualAnnotationsJSON, _ := json.Marshal(cleanedActualAnnotations)
		if string(dryRunAnnotationsJSON) != string(actualAnnotationsJSON) {
			return false
		}
	}

	return true
}

// initializeGVRCache initializes the GVR cache using discovery client
func initializeGVRCache() {
	log.Debug().Msg("Initializing GVR cache using discovery client")

	// Create discovery client using the existing Kubernetes client
	discoveryClient := client.Discovery()

	// Get server resources
	apiResourceLists, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to get server resources")
		return
	}

	gvrCacheLock.Lock()
	defer gvrCacheLock.Unlock()

	resourceCount := 0
	for _, apiResourceList := range apiResourceLists {
		if apiResourceList == nil {
			continue
		}

		gv, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			log.Warn().Err(err).Str("groupVersion", apiResourceList.GroupVersion).Msg("Failed to parse group version")
			continue
		}

		for _, apiResource := range apiResourceList.APIResources {
			// Skip subresources (they contain '/')
			if strings.Contains(apiResource.Name, "/") {
				continue
			}

			gvr := schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: apiResource.Name,
			}

			gvrCache[apiResource.Kind] = gvr
			resourceCount++

			log.Debug().
				Str("kind", apiResource.Kind).
				Str("group", gv.Group).
				Str("version", gv.Version).
				Str("resource", apiResource.Name).
				Bool("namespaced", apiResource.Namespaced).
				Msg("Cached GVR mapping")
		}
	}

	log.Info().Int("resourceCount", resourceCount).Msg("Successfully initialized GVR cache from discovery")
}

// getGVRForKind returns the GroupVersionResource for a given Kind using discovery
func getGVRForKind(kind string) (schema.GroupVersionResource, error) {
	// Initialize cache once
	gvrCacheInit.Do(initializeGVRCache)

	gvrCacheLock.RLock()
	gvr, exists := gvrCache[kind]
	gvrCacheLock.RUnlock()

	if !exists {
		return schema.GroupVersionResource{}, fmt.Errorf("unknown kind: %s", kind)
	}

	return gvr, nil
}

// processGitOpsMismatch handles when a resource doesn't match between Git and cluster
func processGitOpsMismatch(repoState *gitOpsRepositoryState, expected, actual *unstructured.Unstructured, mismatchType string) error {
	kind := expected.GetKind()
	name := expected.GetName()
	namespace := expected.GetNamespace()

	key := fmt.Sprintf("%s/%s/%s/%s", repoState.name, namespace, kind, name)

	log.Error().
		Str("repository", repoState.name).
		Str("kind", kind).
		Str("name", name).
		Str("namespace", namespace).
		Str("mismatchType", mismatchType).
		Msg("GitOps mismatch detected")

	// Update GitOps state
	updateGitOpsState(key, true, fmt.Sprintf("Resource %s: %s", mismatchType, getResourceDescription(expected, actual, mismatchType)),
		repoState.name, kind, name, namespace, mismatchType, "", "")

	// Check if we should send an alert
	if shouldSendGitOpsAlert(key) {
		sendGitOpsMismatchAlert(repoState.name, expected, actual, mismatchType)
		markGitOpsAlertSent(key)
	}

	return nil
}

// processGitOpsMatch handles when a resource matches between Git and cluster
func processGitOpsMatch(repoState *gitOpsRepositoryState, manifest *unstructured.Unstructured) error {
	kind := manifest.GetKind()
	name := manifest.GetName()
	namespace := manifest.GetNamespace()

	key := fmt.Sprintf("%s/%s/%s/%s", repoState.name, namespace, kind, name)

	// Check for recovery
	checkGitOpsRecovery(key, repoState.name, kind, name, namespace)

	// Update state to indicate no error
	updateGitOpsState(key, false, "", repoState.name, kind, name, namespace, "", "", "")

	return nil
}

// getResourceDescription creates a human-readable description of the resource mismatch
func getResourceDescription(expected, actual *unstructured.Unstructured, mismatchType string) string {
	switch mismatchType {
	case "missing":
		return fmt.Sprintf("%s/%s is missing from cluster", expected.GetKind(), expected.GetName())
	case "different":
		return fmt.Sprintf("%s/%s differs between Git and cluster", expected.GetKind(), expected.GetName())
	case "extra":
		return fmt.Sprintf("%s/%s exists in cluster but not in Git", actual.GetKind(), actual.GetName())
	default:
		return fmt.Sprintf("Unknown mismatch type: %s", mismatchType)
	}
}
