package main

import (
	"context"
	"os"
	"time"

	log "github.com/rs/zerolog/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

func runLeaderElection(ctx context.Context) {
	// Ensure namespace is set (only for leader election)
	namespacePod := detectNamespace()
	log.Info().Str("namespace", namespacePod).Msg("Defaulted namespace from POD_NAMESPACE or serviceaccount file (leader election)")

	// Get the pod name from environment variable
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		log.Error().Msg("POD_NAME environment variable not set")
		return
	}

	// Create a new lock
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "sun-leader",
			Namespace: namespacePod,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: podName,
		},
	}

	// Start the leader election code loop
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				leaderLock.Lock()
				isLeader = true
				leaderLock.Unlock()
				log.Info().Msg("Started leading")
			},
			OnStoppedLeading: func() {
				leaderLock.Lock()
				isLeader = false
				leaderLock.Unlock()
				log.Info().Msg("Stopped leading")
			},
			OnNewLeader: func(identity string) {
				log.Info().Str("leader", identity).Msg("New leader elected")
			},
		},
	})
}
