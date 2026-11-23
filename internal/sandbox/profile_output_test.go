package sandbox

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestProfileOutputCopyBound(t *testing.T) {
	b := limitedProfileOutput{limit: 8 << 20}
	_, err := io.Copy(&b, io.LimitReader(bytes.NewReader(bytes.Repeat([]byte("x"), 9<<20)), 9<<20))
	if err == nil || b.buffer.Len() > 8<<20 {
		t.Fatal("capture limit failed")
	}
}

func TestProfileOutputChild(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-3] != "profile-output-child" {
		return
	}
	mode := os.Args[len(os.Args)-2]
	if mode == "small" {
		os.Stdout.Write([]byte("synthetic output"))
		os.Exit(0)
	}
	if err := os.WriteFile(os.Args[len(os.Args)-1], []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(2)
	}
	block := bytes.Repeat([]byte("x"), 64<<10)
	for i := 0; i < 160; i++ {
		_, _ = os.Stdout.Write(block)
	}
	time.Sleep(20 * time.Second)
	os.Exit(0)
}

func TestProfileOutputSubprocess(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a := NewAppleContainer(binary, nil)
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	started := time.Now()
	out, _, err := a.runProfile(context.Background(), "-test.run=^TestProfileOutputChild$", "--", "profile-output-child", "large", pidPath)
	if err == nil || len(out) > 8<<20 {
		t.Fatal("oversized child output accepted")
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("child cleanup was delayed")
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatal("child remains alive")
	}
	out, _, err = a.runProfile(context.Background(), "-test.run=^TestProfileOutputChild$", "--", "profile-output-child", "small", pidPath)
	if err != nil || string(out) != "synthetic output" {
		t.Fatal("small child output failed")
	}
}
