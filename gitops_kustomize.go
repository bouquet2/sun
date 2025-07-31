package main

import (
	"fmt"
	"os"
	"path/filepath"

	log "github.com/rs/zerolog/log"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

// generateKustomizeManifests generates Kubernetes manifests using Kustomize
func generateKustomizeManifests(repoState *gitOpsRepositoryState) ([]*unstructured.Unstructured, error) {
	repoState.mutex.RLock()
	defer repoState.mutex.RUnlock()

	// Get the repository configuration
	var repoConfig *GitOpsRepository
	for _, repo := range config.GitOps.Repositories {
		if repo.Name == repoState.name {
			repoConfig = &repo
			break
		}
	}
	if repoConfig == nil {
		return nil, fmt.Errorf("repository configuration not found for %s", repoState.name)
	}

	// Build the path to the kustomization
	kustomizePath := filepath.Join(repoState.localPath, repoState.path)

	log.Debug().
		Str("repository", repoState.name).
		Str("path", kustomizePath).
		Msg("Generating Kustomize manifests")

	// Get Helm command if configured
	helmCommand := repoConfig.Kustomize.HelmCommand
	if helmCommand == "" {
		helmCommand = "helm" // Default to "helm"
	}

	log.Debug().
		Str("repository", repoState.name).
		Str("helmCommand", helmCommand).
		Msg("Configured Helm command for Kustomize")

	// Create filesystem
	fSys := filesys.MakeFsOnDisk()

	// Check if kustomization.yaml or kustomization.yml exists
	kustomizationFile := ""
	for _, filename := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		fullPath := filepath.Join(kustomizePath, filename)
		if fSys.Exists(fullPath) {
			kustomizationFile = fullPath
			break
		}
	}

	if kustomizationFile == "" {
		return nil, fmt.Errorf("no kustomization file found in %s", kustomizePath)
	}

	log.Debug().
		Str("repository", repoState.name).
		Str("kustomizationFile", kustomizationFile).
		Msg("Found kustomization file")

	// Create Kustomize options with proper plugin configuration
	pluginConfig := types.EnabledPluginConfig(types.BploUseStaticallyLinked)
	pluginConfig.HelmConfig.Command = helmCommand

	// Copy .env.example to .env if configured and .env.example exists
	if repoConfig.Kustomize.CopyEnvExample {
		if err := copyEnvExampleFiles(kustomizePath, repoState.name); err != nil {
			log.Warn().
				Err(err).
				Str("repository", repoState.name).
				Msg("Failed to copy .env.example files")
		}
	}

	opts := &krusty.Options{
		LoadRestrictions:  types.LoadRestrictionsNone,
		AddManagedbyLabel: false,
		PluginConfig:      pluginConfig,
		Reorder:           krusty.ReorderOptionUnspecified, // Let kustomization.yaml sortOptions take precedence
	}

	// Build the manifests
	k := krusty.MakeKustomizer(opts)
	resMap, err := k.Run(fSys, kustomizePath)
	if err != nil {
		return nil, fmt.Errorf("failed to run kustomize for repository %s: %w", repoState.name, err)
	}

	// Convert to unstructured objects
	var manifests []*unstructured.Unstructured
	for _, res := range resMap.Resources() {
		// Get the resource as YAML
		yamlBytes, err := res.AsYAML()
		if err != nil {
			log.Error().
				Err(err).
				Str("repository", repoState.name).
				Str("resource", res.CurId().String()).
				Msg("Failed to convert resource to YAML")
			continue
		}

		// Convert to unstructured
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(yamlBytes); err != nil {
			// Try YAML unmarshaling if JSON fails
			if err := yaml.Unmarshal(yamlBytes, &obj.Object); err != nil {
				log.Error().
					Err(err).
					Str("repository", repoState.name).
					Str("resource", res.CurId().String()).
					Msg("Failed to unmarshal resource")
				continue
			}
		}

		// Apply filtering based on allowlist/denylist
		if shouldFilterResource(obj) {
			log.Debug().
				Str("repository", repoState.name).
				Str("kind", obj.GetKind()).
				Str("name", obj.GetName()).
				Str("namespace", obj.GetNamespace()).
				Msg("Resource filtered out by allowlist/denylist")
			continue
		}

		manifests = append(manifests, obj)

		log.Debug().
			Str("repository", repoState.name).
			Str("kind", obj.GetKind()).
			Str("name", obj.GetName()).
			Str("namespace", obj.GetNamespace()).
			Msg("Generated manifest")
	}

	log.Info().
		Str("repository", repoState.name).
		Int("manifests", len(manifests)).
		Msg("Successfully generated Kustomize manifests")

	return manifests, nil
}

// shouldFilterResource checks if a resource should be filtered based on allowlist/denylist
func shouldFilterResource(obj *unstructured.Unstructured) bool {
	kind := obj.GetKind()
	namespace := obj.GetNamespace()

	// Check denylist first (takes precedence)
	if len(config.GitOps.Denylist.Kinds) > 0 {
		for _, deniedKind := range config.GitOps.Denylist.Kinds {
			if kind == deniedKind {
				return true // Filter out
			}
		}
	}

	if len(config.GitOps.Denylist.Namespaces) > 0 {
		for _, deniedNamespace := range config.GitOps.Denylist.Namespaces {
			if namespace == deniedNamespace {
				return true // Filter out
			}
		}
	}

	// Check allowlist (if specified, only allow listed items)
	if len(config.GitOps.Allowlist.Kinds) > 0 {
		allowed := false
		for _, allowedKind := range config.GitOps.Allowlist.Kinds {
			if kind == allowedKind {
				allowed = true
				break
			}
		}
		if !allowed {
			return true // Filter out
		}
	}

	if len(config.GitOps.Allowlist.Namespaces) > 0 {
		allowed := false
		for _, allowedNamespace := range config.GitOps.Allowlist.Namespaces {
			if namespace == allowedNamespace {
				allowed = true
				break
			}
		}
		if !allowed {
			return true // Filter out
		}
	}

	return false // Don't filter
}

// copyEnvExampleFiles copies .env.example to .env in the specified directory and subdirectories
func copyEnvExampleFiles(rootPath, repositoryName string) error {
	log.Debug().
		Str("repository", repositoryName).
		Str("path", rootPath).
		Msg("Looking for .env.example files to copy")

	copiedCount := 0
	skippedCount := 0

	err := filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip if not a .env.example file
		if info.IsDir() || info.Name() != ".env.example" {
			return nil
		}

		// Determine the target .env file path
		envPath := filepath.Join(filepath.Dir(path), ".env")

		// Check if .env already exists
		if _, err := os.Stat(envPath); err == nil {
			skippedCount++
			log.Debug().
				Str("repository", repositoryName).
				Str("envPath", envPath).
				Msg(".env file already exists, skipping copy")
			return nil
		}

		// Copy .env.example to .env
		log.Debug().
			Str("repository", repositoryName).
			Str("source", path).
			Str("target", envPath).
			Msg("Copying .env.example to .env")

		sourceData, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read .env.example file %s: %w", path, err)
		}

		if err := os.WriteFile(envPath, sourceData, 0644); err != nil {
			return fmt.Errorf("failed to write .env file %s: %w", envPath, err)
		}

		copiedCount++
		return nil
	})

	// Log summary
	if copiedCount > 0 || skippedCount > 0 {
		log.Info().
			Str("repository", repositoryName).
			Int("copied", copiedCount).
			Int("skipped", skippedCount).
			Msg("Processed .env.example files")
	}

	return err
}
