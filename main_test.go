package main

import "testing"

func TestParseFlagsStopsAtTheCommand(t *testing.T) {
	// Everything after the first non-flag belongs to the remote command, so
	// `on host claude --resume` sends --resume to claude rather than to on.
	o, rest, err := parseFlags([]string{"-d", "claude", "--resume"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.detach {
		t.Error("-d should be parsed")
	}
	if len(rest) != 2 || rest[0] != "claude" || rest[1] != "--resume" {
		t.Errorf("command should pass through untouched, got %v", rest)
	}
}

func TestParseFlagsDoubleDashEndsFlags(t *testing.T) {
	_, rest, err := parseFlags([]string{"-d", "--", "--version"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0] != "--version" {
		t.Errorf("-- should end flag parsing, got %v", rest)
	}
}

func TestParseFlagsRejectsUnknown(t *testing.T) {
	if _, _, err := parseFlags([]string{"--nope"}); err == nil {
		t.Error("unknown flags should be rejected, not passed through")
	}
}

func TestMergeOverlaysFlagsGivenAfterTheHost(t *testing.T) {
	// Regression: flags were only accepted before the host, so
	// `on host --repo myapp claude` silently passed "--repo myapp" to the remote
	// command — which the usage text advertised as supported.
	before, _, _ := parseFlags([]string{"-d"})
	after, rest, err := parseFlags([]string{"--repo", "myapp", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	got := merge(before, after)
	if !got.detach {
		t.Error("flag from before the host was lost")
	}
	if got.repo != "myapp" {
		t.Errorf("flag from after the host was lost: %+v", got)
	}
	if len(rest) != 1 || rest[0] != "claude" {
		t.Errorf("command should be just the command, got %v", rest)
	}
}
