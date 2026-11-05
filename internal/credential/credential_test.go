package credential

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeKeychain struct {
	account string
	value   string
	err     error
}

func (f *fakeKeychain) Get(_ context.Context, account string) (string, error) {
	f.account = account
	return f.value, f.err
}

func TestParse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		spec     string
		opPath   string
		wantDesc string
		wantErr  bool
	}{
		{name: "empty means keychain", spec: "", wantDesc: "keychain:anthropic"},
		{name: "keychain", spec: "keychain", wantDesc: "keychain:anthropic"},
		{name: "keychain with account", spec: "keychain:other", wantDesc: "keychain:other"},
		{name: "keychain with blank account", spec: "keychain:", wantErr: true},
		{name: "1password", spec: "op://Private/Claude/token", opPath: "/usr/bin/op", wantDesc: "op://Private/Claude/token"},
		// A reference the user configured, with no op installed, has to say so
		// rather than silently falling back to a different store.
		{name: "1password without op", spec: "op://Private/Claude/token", wantErr: true},
		{name: "command", spec: "cmd:echo hi", wantDesc: "cmd"},
		{name: "blank command", spec: "cmd:", wantErr: true},
		{name: "nonsense", spec: "vault-of-secrets", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			source, err := Parse(tc.spec, "anthropic", &fakeKeychain{}, tc.opPath)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) error = nil, want an error", tc.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tc.spec, err)
			}
			if got := source.Describe(); got != tc.wantDesc {
				t.Errorf("Describe() = %q, want %q", got, tc.wantDesc)
			}
		})
	}
}

// Describe is printed by doctor and in errors, so it must never be the secret.
func TestCommandDescribeHidesTheCommand(t *testing.T) {
	t.Parallel()

	source, err := Parse("cmd:cat /home/someone/private/token", "anthropic", &fakeKeychain{}, "")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if strings.Contains(source.Describe(), "private") {
		t.Errorf("Describe() = %q, want it to omit the command", source.Describe())
	}
}

func TestKeychainSourceUsesTheAccount(t *testing.T) {
	t.Parallel()

	kc := &fakeKeychain{value: "secret"}
	source, err := Parse("keychain:claude-subscription", "anthropic", kc, "")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	got, err := source.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got != "secret" {
		t.Errorf("Get() = %q, want %q", got, "secret")
	}
	if kc.account != "claude-subscription" {
		t.Errorf("keychain account = %q, want claude-subscription", kc.account)
	}
}

func TestKeychainSourceSurfacesErrors(t *testing.T) {
	t.Parallel()

	kc := &fakeKeychain{err: errors.New("keychain is locked")}
	source, _ := Parse("keychain", "anthropic", kc, "")
	if _, err := source.Get(context.Background()); err == nil {
		t.Fatal("Get() error = nil, want the keychain error")
	}
}

func script(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func TestCommandSource(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{name: "trims trailing newline", body: `echo "  secret  "`, want: "secret"},
		{name: "empty output", body: "true", wantErr: "returned nothing"},
		{name: "non-zero exit", body: "echo 'vault locked' >&2; exit 1", wantErr: "vault locked"},
		// A script that prints diagnostics alongside the secret would otherwise
		// have the whole blob used as a credential, producing a 401 that is very
		// hard to trace back here.
		{name: "multi-line output", body: `printf 'note\nsecret\n'`, wantErr: "more than one line"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			source, err := Parse("cmd:"+script(t, tc.body), "anthropic", nil, "")
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			got, err := source.Get(context.Background())
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Get() error = nil, want one containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Get() error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("Get() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Stdout is the secret, so it must never end up in an error message.
func TestCommandErrorsCarryNoStdout(t *testing.T) {
	t.Parallel()

	source, _ := Parse("cmd:"+script(t, `echo super-secret-value; echo failed >&2; exit 3`), "anthropic", nil, "")
	_, err := source.Get(context.Background())
	if err == nil {
		t.Fatal("Get() error = nil, want an error")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Errorf("error leaked stdout: %v", err)
	}
}

// A cancelled context must stop a session start from hanging on a helper.
func TestCommandRespectsCancellation(t *testing.T) {
	t.Parallel()

	source, _ := Parse("cmd:sleep 30", "anthropic", nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := source.Get(ctx); err == nil {
		t.Fatal("Get() error = nil, want a cancellation error")
	}
}
