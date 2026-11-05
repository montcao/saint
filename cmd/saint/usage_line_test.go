package main

import (
	"strings"
	"testing"

	"github.com/montcao/saint/internal/proxy"
)

func TestUsageLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		event proxy.UsageEvent
		want  string
	}{
		{
			name: "streamed anthropic request",
			event: proxy.UsageEvent{
				SessionID: "abc123",
				Provider:  "anthropic",
				Model:     "claude-opus-5",
				Stream:    true,
				Usage: proxy.Usage{
					InputTokens: 1200, OutputTokens: 340,
					CacheReadTokens: 900, CacheCreationTokens: 64,
					Reported: true,
				},
			},
			want: "usage session=abc123 anthropic claude-opus-5 stream in=1200 out=340 cache_read=900 cache_write=64",
		},
		{
			name: "cancelled generation",
			event: proxy.UsageEvent{
				SessionID: "abc123",
				Provider:  "anthropic",
				Model:     "claude-opus-5",
				Stream:    true,
				Partial:   true,
				Usage:     proxy.Usage{InputTokens: 90, OutputTokens: 7, Reported: true},
			},
			want: "usage session=abc123 anthropic claude-opus-5 stream partial in=90 out=7 cache_read=0 cache_write=0",
		},
		{
			// An upstream that reports nothing must not be rendered as a request
			// that cost nothing.
			name: "usage not reported",
			event: proxy.UsageEvent{
				SessionID: "abc123",
				Provider:  "openai",
				Stream:    true,
			},
			want: "usage session=abc123 openai unknown-model stream tokens=unreported",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := usageLine(tc.event); got != tc.want {
				t.Errorf("usageLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The log is written to disk and read by humans. Nothing that identifies a
// credential may reach it.
func TestUsageLineCarriesNoSecrets(t *testing.T) {
	t.Parallel()

	line := usageLine(proxy.UsageEvent{
		SessionID: "abc123",
		Provider:  "anthropic",
		Model:     "claude-opus-5",
		Usage:     proxy.Usage{InputTokens: 1, Reported: true},
	})
	for _, field := range []string{"token", "key", "authorization"} {
		if strings.Contains(strings.ToLower(line), field) {
			t.Errorf("usageLine() = %q, must not mention %q", line, field)
		}
	}
}
