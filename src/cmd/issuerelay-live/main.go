// Temporary live watcher runner (stand-in until the new image rolls).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/github"
)

func main() {
	tok, err := os.ReadFile("/var/run/hive-metrics/gh-app-token.cache")
	if err != nil {
		fmt.Fprintln(os.Stderr, "read token cache:", err)
		os.Exit(1)
	}
	org := os.Getenv("LIVE_ORG")
	repos := strings.Split(os.Getenv("LIVE_REPOS"), ",")
	apiURL := os.Getenv("LIVE_API_URL")
	expUID, _ := strconv.Atoi(os.Getenv("LIVE_SECCHECK_UID"))
	mins, _ := strconv.Atoi(os.Getenv("LIVE_MINUTES"))
	if mins <= 0 {
		mins = 60
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := github.NewClient(strings.TrimSpace(string(tok)), org, repos, logger, apiURL)
	authz := func(agent string, fileUID int, kind string) error {
		if agent != "sec-check" || fileUID != expUID {
			return fmt.Errorf("live authz: agent=%s uid=%d not allowed", agent, fileUID)
		}
		return nil
	}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	c.StartIssueRequestWatcher(done, authz, nil)
	deadline := time.Now().Add(time.Duration(mins) * time.Minute)
	for time.Now().Before(deadline) {
		c.ProcessIssueRequestsOnce(context.Background())
		time.Sleep(5 * time.Second)
	}
}
