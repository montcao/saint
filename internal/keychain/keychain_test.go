package keychain

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// exitErrorWithCode returns a real *exec.ExitError carrying the requested exit
// status. A zero-valued &exec.ExitError{} reports -1, which is indistinguishable
// from "the binary never started".
func exitErrorWithCode(t *testing.T, code int) error {
	t.Helper()

	err := exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(code)).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("want *exec.ExitError, got %T (%v)", err, err)
	}
	if exitErr.ExitCode() != code {
		t.Fatalf("exit code = %d, want %d", exitErr.ExitCode(), code)
	}
	return err
}

func TestGetUsesExpectedArguments(t *testing.T) {
	t.Parallel()

	k := New("/usr/bin/security", nil)
	k.exec = func(_ context.Context, bin string, stdin []byte, args ...string) ([]byte, []byte, error) {
		if bin != "/usr/bin/security" {
			t.Fatalf("bin = %q", bin)
		}
		if len(stdin) != 0 {
			t.Fatalf("stdin = %q, want empty", stdin)
		}
		want := []string{"find-generic-password", "-s", "saint", "-a", "anthropic", "-w"}
		if len(args) != len(want) {
			t.Fatalf("args = %#v", args)
		}
		for i := range want {
			if args[i] != want[i] {
				t.Fatalf("args[%d] = %q, want %q", i, args[i], want[i])
			}
		}
		return []byte("secret\n"), nil, nil
	}

	value, err := k.Get(context.Background(), ProviderAnthropic)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if value != "secret" {
		t.Fatalf("value = %q", value)
	}
}

func TestHasClassifiesExitStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		want    bool
		wantErr bool
	}{
		{name: "key present", err: nil, want: true},
		{name: "item not found", err: exitErrorWithCode(t, exitItemNotFound), want: false},
		{name: "locked keychain", err: exitErrorWithCode(t, 36), wantErr: true},
		{name: "user denied", err: exitErrorWithCode(t, 51), wantErr: true},
		{name: "binary missing", err: errors.New("exec: no such file"), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			k := New("/usr/bin/security", nil)
			k.exec = func(_ context.Context, _ string, _ []byte, _ ...string) ([]byte, []byte, error) {
				return []byte("value\n"), nil, tc.err
			}

			got, err := k.Has(context.Background(), ProviderOpenAI)
			if tc.wantErr != (err != nil) {
				t.Fatalf("Has() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("Has() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSetDeliversSecretOnStdinNotArgv(t *testing.T) {
	t.Parallel()

	const secret = "sk-ant-super-secret-value"

	var gotStdin string
	var gotArgs []string
	k := New("/usr/bin/security", nil)
	k.exec = func(_ context.Context, _ string, stdin []byte, args ...string) ([]byte, []byte, error) {
		gotStdin = string(stdin)
		gotArgs = append([]string(nil), args...)
		return nil, nil, nil
	}

	if err := k.Set(context.Background(), ProviderOpenAI, secret); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// security(1) prompts twice: "password data" then "retype password".
	if gotStdin != secret+"\n"+secret+"\n" {
		t.Fatalf("stdin = %q", gotStdin)
	}
	for _, arg := range gotArgs {
		if strings.Contains(arg, secret) {
			t.Fatalf("secret leaked into argv: %#v", gotArgs)
		}
	}
	if gotArgs[len(gotArgs)-1] != "-w" {
		t.Fatalf("-w must be the last argument for the stdin prompt: %#v", gotArgs)
	}
}

func TestSetErrorNeverContainsSecret(t *testing.T) {
	t.Parallel()

	const secret = "sk-ant-super-secret-value"

	tests := []struct {
		name string
		err  error
	}{
		{name: "plain failure", err: errors.New("security add-generic-password: exit status 45")},
		{name: "stderr echoes secret", err: errors.New("security add-generic-password: exit status 1: bad value " + secret)},
		{name: "argv in error", err: errors.New("add-generic-password -U -s saint -a openai -w " + secret + ": exit status 1")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			k := New("/usr/bin/security", nil)
			k.exec = func(_ context.Context, _ string, _ []byte, _ ...string) ([]byte, []byte, error) {
				return nil, nil, tc.err
			}

			err := k.Set(context.Background(), ProviderAnthropic, secret)
			if err == nil {
				t.Fatal("Set() error = nil, want non-nil")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked secret: %q", err.Error())
			}
		})
	}
}

func TestSetRejectsUnusableValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "newline", value: "abc\ndef"},
		{name: "carriage return", value: "abc\rdef"},
		{name: "nul", value: "abc\x00def"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			k := New("/usr/bin/security", nil)
			k.exec = func(_ context.Context, _ string, _ []byte, _ ...string) ([]byte, []byte, error) {
				t.Fatal("exec must not run for an unusable value")
				return nil, nil, nil
			}

			if err := k.Set(context.Background(), ProviderAnthropic, tc.value); err == nil {
				t.Fatal("Set() error = nil, want non-nil")
			}
		})
	}
}

func TestRunCommandErrorOmitsArgv(t *testing.T) {
	t.Parallel()

	const secret = "sk-live-do-not-print-me"

	_, _, err := runCommand(context.Background(), "/bin/sh", nil, "-c", "exit 45", secret)
	if err == nil {
		t.Fatal("runCommand() error = nil, want non-nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked argv: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "exit status 45") {
		t.Fatalf("error = %q, want exit status", err.Error())
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error does not unwrap to *exec.ExitError: %v", err)
	}
}

func TestRunCommandWritesStdin(t *testing.T) {
	t.Parallel()

	stdout, _, err := runCommand(context.Background(), "/bin/cat", []byte("line-one\nline-two\n"))
	if err != nil {
		t.Fatalf("runCommand() error = %v", err)
	}
	if string(stdout) != "line-one\nline-two\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestRemovePropagatesError(t *testing.T) {
	t.Parallel()

	k := New("/usr/bin/security", nil)
	k.exec = func(_ context.Context, _ string, _ []byte, _ ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("boom")
	}

	if err := k.Remove(context.Background(), ProviderAnthropic); err == nil {
		t.Fatal("Remove() error = nil, want non-nil")
	}
}
