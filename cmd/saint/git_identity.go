package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// hostGitIdentity reads user.name and user.email from the user's global git
// configuration, for the guest's commits. Nothing else from that
// configuration reaches the guest.
func hostGitIdentity() (name, email string) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return "", ""
	}
	return globalGitValue(gitPath, "user.name"), globalGitValue(gitPath, "user.email")
}

func globalGitValue(gitPath, key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitPath, "config", "--global", "--includes", "--get", key)
	// --global reads only the user's file, but running outside any repository
	// keeps git from discovering one on the way.
	cmd.Dir = os.TempDir()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
