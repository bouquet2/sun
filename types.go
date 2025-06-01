package main

import (
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
)

const version = "0.0.9"

var isLeader bool
var leaderLock sync.RWMutex
var config Config
var client *kubernetes.Clientset

type Config struct {
	WebhookUrl string `mapstructure:"webhook_url"`
	Namespace  string `mapstructure:"namespace"`
	LogLevel   string `mapstructure:"log_level"`
	Interval   int    `mapstructure:"interval"` // Interval in minutes
}

type Alert struct {
	Title       string
	Description string
	Fields      []struct {
		Name   string
		Value  string
		Inline bool
	}
	Logs string // Add logs field
}

type unitState struct {
	hasError    bool
	lastSeen    time.Time
	lastMessage string
	firstError  time.Time // When the error was first seen
	alertSent   bool      // Whether we've sent an alert for the current error state
}
