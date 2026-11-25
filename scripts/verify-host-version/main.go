package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/montcao/saint/internal/claudeimage"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/session"
	"github.com/montcao/saint/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func main() {
	live := flag.Bool("live", false, "build/verify the exact host Claude image and create two disposable microVM sessions; no credentials or model calls")
	flag.Parse()
	if !*live || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/verify-host-version -live (uses the installed Apple container service; preserves shared builder/cache)")
		os.Exit(2)
	}
	if e := verify(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func verify() (resultErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	home, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	root, e := os.MkdirTemp(home, "saint-version-verification-")
	if e != nil {
		return e
	}
	if e = os.Chmod(root, 0700); e != nil {
		return e
	}
	repo := filepath.Join(root, "project")
	if e = os.Mkdir(repo, 0700); e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(repo, "README.md"), []byte("Synthetic version verification fixture.\n"), 0600); e != nil {
		return e
	}
	st := store.New(root)
	if _, e = st.EnsureConfig(); e != nil {
		return e
	}
	bin, e := exec.LookPath("container")
	if e != nil {
		return e
	}
	m := &claudeimage.Manager{Container: bin, LockDir: filepath.Join(st.Paths().Root, "image-locks"), Progress: os.Stdout}
	sb := sandbox.NewAppleContainer(bin, nil)
	sb.ClaudeImages = m
	svc := &session.Service{Store: st, Sandbox: sb, Workdir: repo}
	var owned []string
	defer func() {
		for _, id := range owned {
			cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
			e := svc.Delete(cleanup, id)
			done()
			fmt.Printf("owned session cleanup %s: %v\n", id, e)
			resultErr = errors.Join(resultErr, e)
		}
	}()
	results := []map[string]string{}
	fmt.Println("PRIVATE_FIXTURE", root)
	for i := 0; i < 2; i++ {
		created, e := svc.Create(ctx, session.CreateRequest{Runtime: sandbox.RuntimeClaude, Repos: []sandbox.Repo{{Path: repo}}, Name: fmt.Sprintf("version-check-%d", i+1), CPUs: 1, MemoryGB: 2})
		if e != nil {
			return e
		}
		owned = append(owned, created.ID)
		// No real credential or upstream service is configured. Only version is queried.
		data, e := exec.CommandContext(ctx, bin, "exec", svc.ContainerName(created.ID), "/usr/local/bin/claude", "--version").Output()
		if e != nil {
			return e
		}
		actual, e := claudeimage.ParseVersion(string(data))
		if e != nil {
			return e
		}
		if actual != created.AgentVersion {
			return fmt.Errorf("session guest mismatch")
		}
		results = append(results, map[string]string{"host_cli_version": created.AgentVersion, "guest_version": actual, "architecture": created.GuestArchitecture, "image": created.Image, "recipe": created.ImageRecipe, "digest": created.ImageDigest})
		b, _ := json.Marshal(results)
		if e = os.WriteFile(filepath.Join(root, "results.json"), b, 0600); e != nil {
			return e
		}
		fmt.Printf("SESSION_RESULT %d host=%s guest=%s architecture=%s image=%s\n", i+1, created.AgentVersion, actual, created.GuestArchitecture, created.Image)
	}
	return nil
}
