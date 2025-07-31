package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	log "github.com/rs/zerolog/log"
)

// GitOps repository state tracking
var (
	gitOpsRepositories     = make(map[string]*gitOpsRepositoryState)
	gitOpsRepositoriesLock sync.RWMutex
)

// setupGitOpsMonitoring sets up GitOps monitoring for configured repositories
func setupGitOpsMonitoring(ctx context.Context) error {
	if !config.GitOps.Enabled {
		log.Info().Msg("GitOps monitoring is disabled")
		return nil
	}

	if len(config.GitOps.Repositories) == 0 {
		log.Info().Msg("No GitOps repositories configured")
		return nil
	}

	log.Info().Int("repositories", len(config.GitOps.Repositories)).Msg("Setting up GitOps monitoring")

	// Create temporary directory for repositories
	tempDir, err := os.MkdirTemp("", "sun-gitops-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}

	// Initialize repository states
	gitOpsRepositoriesLock.Lock()
	for _, repo := range config.GitOps.Repositories {
		if repo.Name == "" || repo.URL == "" {
			log.Warn().Str("name", repo.Name).Str("url", repo.URL).Msg("Skipping repository with missing name or URL")
			continue
		}

		// Set defaults
		path := repo.Path
		if path == "" {
			path = "."
		}
		branch := repo.Branch
		if branch == "" {
			branch = "main"
		}

		localPath := filepath.Join(tempDir, repo.Name)

		// Determine sync interval (repository-specific or global default)
		syncIntervalMinutes := repo.SyncIntervalMinutes
		if syncIntervalMinutes <= 0 {
			syncIntervalMinutes = config.GitOps.SyncIntervalMinutes
		}
		if syncIntervalMinutes <= 0 {
			syncIntervalMinutes = 5 // Fallback default
		}
		syncInterval := time.Duration(syncIntervalMinutes) * time.Minute

		gitOpsRepositories[repo.Name] = &gitOpsRepositoryState{
			name:         repo.Name,
			url:          repo.URL,
			path:         path,
			branch:       branch,
			localPath:    localPath,
			syncInterval: syncInterval,
		}

		log.Debug().
			Str("name", repo.Name).
			Str("url", repo.URL).
			Str("path", path).
			Str("branch", branch).
			Str("localPath", localPath).
			Int("syncIntervalMinutes", syncIntervalMinutes).
			Msg("GitOps repository configured")
	}
	gitOpsRepositoriesLock.Unlock()

	// Start monitoring goroutines for each repository
	for _, repoState := range gitOpsRepositories {
		go monitorGitOpsRepository(ctx, repoState)
	}

	log.Info().Msg("GitOps monitoring started")
	return nil
}

// monitorGitOpsRepository monitors a single GitOps repository
func monitorGitOpsRepository(ctx context.Context, repoState *gitOpsRepositoryState) {
	log.Info().Str("repository", repoState.name).Msg("Starting GitOps repository monitoring")

	// Initial sync
	if err := syncRepository(repoState); err != nil {
		log.Error().Err(err).Str("repository", repoState.name).Msg("Failed initial repository sync")
		return
	}

	// Initial comparison
	if err := compareManifests(repoState); err != nil {
		log.Error().Err(err).Str("repository", repoState.name).Msg("Failed initial manifest comparison")
	}

	// Set up periodic sync
	ticker := time.NewTicker(repoState.syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Str("repository", repoState.name).Msg("Stopping GitOps repository monitoring")
			return
		case <-ticker.C:
			// Check if we're the leader before doing work
			leaderLock.RLock()
			if !isLeader {
				leaderLock.RUnlock()
				continue
			}
			leaderLock.RUnlock()

			log.Debug().Str("repository", repoState.name).Msg("Syncing GitOps repository")

			if err := syncRepository(repoState); err != nil {
				log.Error().Err(err).Str("repository", repoState.name).Msg("Failed to sync repository")
				continue
			}

			if err := compareManifests(repoState); err != nil {
				log.Error().Err(err).Str("repository", repoState.name).Msg("Failed to compare manifests")
			}
		}
	}
}

// syncRepository clones or pulls the latest changes from a Git repository
func syncRepository(repoState *gitOpsRepositoryState) error {
	repoState.mutex.Lock()
	defer repoState.mutex.Unlock()

	log.Debug().Str("repository", repoState.name).Str("url", repoState.url).Msg("Syncing repository")

	// Check if repository already exists locally
	if repoState.repository == nil {
		// Clone repository
		log.Debug().Str("repository", repoState.name).Str("localPath", repoState.localPath).Msg("Cloning repository")

		repo, err := git.PlainClone(repoState.localPath, false, &git.CloneOptions{
			URL:           repoState.url,
			ReferenceName: plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", repoState.branch)),
			SingleBranch:  true,
			Depth:         1, // Shallow clone for efficiency
		})
		if err != nil {
			return fmt.Errorf("failed to clone repository %s: %w", repoState.name, err)
		}

		repoState.repository = repo
		log.Info().Str("repository", repoState.name).Msg("Repository cloned successfully")
	} else {
		// Pull latest changes
		log.Debug().Str("repository", repoState.name).Msg("Pulling latest changes")

		workTree, err := repoState.repository.Worktree()
		if err != nil {
			return fmt.Errorf("failed to get worktree for repository %s: %w", repoState.name, err)
		}

		err = workTree.Pull(&git.PullOptions{
			ReferenceName: plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", repoState.branch)),
			SingleBranch:  true,
		})
		if err != nil && err != git.NoErrAlreadyUpToDate {
			log.Warn().
				Err(err).
				Str("repository", repoState.name).
				Msg("Failed to pull repository, attempting to re-clone")

			// Remove the corrupted local repository
			if err := os.RemoveAll(repoState.localPath); err != nil {
				log.Warn().
					Err(err).
					Str("repository", repoState.name).
					Str("localPath", repoState.localPath).
					Msg("Failed to remove corrupted repository directory")
			}

			// Reset repository state and re-clone
			repoState.repository = nil
			repo, err := git.PlainClone(repoState.localPath, false, &git.CloneOptions{
				URL:           repoState.url,
				ReferenceName: plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", repoState.branch)),
				SingleBranch:  true,
				Depth:         1, // Shallow clone for efficiency
			})
			if err != nil {
				return fmt.Errorf("failed to re-clone repository %s after pull failure: %w", repoState.name, err)
			}

			repoState.repository = repo
			log.Info().
				Str("repository", repoState.name).
				Msg("Repository re-cloned successfully after pull failure")
		} else if err == git.NoErrAlreadyUpToDate {
			log.Debug().Str("repository", repoState.name).Msg("Repository already up to date")
		} else {
			log.Debug().Str("repository", repoState.name).Msg("Repository updated")
		}
	}

	// Get current commit hash
	ref, err := repoState.repository.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD for repository %s: %w", repoState.name, err)
	}

	currentCommit := ref.Hash().String()
	if currentCommit != repoState.lastCommit {
		log.Info().
			Str("repository", repoState.name).
			Str("commit", currentCommit[:8]).
			Msg("Repository updated to new commit")
		repoState.lastCommit = currentCommit
	}

	repoState.lastSync = time.Now()
	return nil
}
