package quotahistory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestRecorder(t *testing.T, now *time.Time) *Recorder {
	t.Helper()
	recorder, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	recorder.now = func() time.Time { return *now }
	return recorder
}

func TestParseWindowsClaudeConvertsFractionsToPercent(t *testing.T) {
	windows := ParseWindows("claude", map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization":        "0.02",
		"Anthropic-Ratelimit-Unified-5h-Reset":              "1793224800",
		"Anthropic-Ratelimit-Unified-7d-Utilization":        "0.45",
		"Anthropic-Ratelimit-Unified-7d-Reset":              "1793451600",
		"Anthropic-Ratelimit-Unified-Status":                "allowed",
		"Anthropic-Ratelimit-Unified-Fallback-Percentage":   "0.5",
		"Anthropic-Ratelimit-Unified-Representative-Claim":  "five_hour",
		"Anthropic-Ratelimit-Unified-7d_sonnet-Utilization": "bogus",
	}, time.Unix(1793200000, 0))
	want := []Window{{ID: "5h", Used: 2, ResetAt: 1793224800}, {ID: "7d", Used: 45, ResetAt: 1793451600}}
	if !sameWindows(windows, want) {
		t.Fatalf("windows = %#v, want %#v", windows, want)
	}
}

func TestParseWindowsCodexKeepsRealWindowsAndDerivesResets(t *testing.T) {
	observed := time.Unix(1793200000, 0)
	windows := ParseWindows("codex", map[string]string{
		"X-Codex-Primary-Used-Percent":                  "11",
		"X-Codex-Primary-Window-Minutes":                "10080",
		"X-Codex-Primary-Reset-At":                      "1793774149",
		"X-Codex-Secondary-Used-Percent":                "0",
		"X-Codex-Secondary-Window-Minutes":              "0",
		"X-Codex-Secondary-Reset-After-Seconds":         "0",
		"X-Codex-Bengalfox-Primary-Used-Percent":        "40",
		"X-Codex-Bengalfox-Primary-Reset-After-Seconds": "3600",
		"X-Codex-Plan-Type":                             "pro",
	}, observed)
	want := []Window{
		{ID: "bengalfox-primary", Used: 40, ResetAt: observed.Unix() + 3600},
		{ID: "primary", Used: 11, ResetAt: 1793774149, Minutes: 10080},
	}
	if !sameWindows(windows, want) {
		t.Fatalf("windows = %#v, want %#v", windows, want)
	}
}

func TestParseWindowsIgnoresOtherProviders(t *testing.T) {
	if got := ParseWindows("xai", map[string]string{"X-Codex-Primary-Used-Percent": "5"}, time.Now()); len(got) != 0 {
		t.Fatalf("windows = %#v, want none", got)
	}
}

func TestRecorderCoalescesAndSkipsUnchangedReadings(t *testing.T) {
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	recorder := newTestRecorder(t, &now)
	reading := func(used string) map[string]string {
		return map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": used,
			"Anthropic-Ratelimit-Unified-7d-Reset":       "1793451600",
		}
	}
	recorder.Observe("a", "idx-a", "claude-a.json", "claude", reading("0.40"), now)
	recorder.Observe("a", "idx-a", "claude-a.json", "claude", reading("0.41"), now.Add(5*time.Second))
	recorder.Flush()
	recorder.Observe("a", "idx-a", "claude-a.json", "claude", reading("0.41"), now.Add(40*time.Second))
	recorder.Flush()
	recorder.Observe("a", "idx-a", "claude-a.json", "claude", reading("0.43"), now.Add(80*time.Second))
	recorder.Observe("b", "idx-b", "codex-b.json", "codex", map[string]string{
		"X-Codex-Primary-Used-Percent":   "10",
		"X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Primary-Reset-At":       "1793774149",
	}, now.Add(81*time.Second))
	recorder.Flush()

	samples, err := recorder.Read(now.Add(-time.Hour), now.Add(time.Hour), "")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %#v, want 3 (coalesced 0.41, changed 0.43, codex)", samples)
	}
	if samples[0].Windows[0].Used != 41 || samples[0].At != now.Add(5*time.Second).Unix() {
		t.Fatalf("first sample = %#v, want the latest reading of the first interval", samples[0])
	}
	if samples[1].Windows[0].Used != 43 || samples[1].Name != "claude-a.json" || samples[1].AuthIndex != "idx-a" {
		t.Fatalf("second sample = %#v", samples[1])
	}
	codex, err := recorder.Read(now.Add(-time.Hour), now.Add(time.Hour), "codex")
	if err != nil || len(codex) != 1 || codex[0].Auth != "b" {
		t.Fatalf("codex samples = %#v err=%v", codex, err)
	}
}

func TestRecorderReadsAcrossDaysWithinRange(t *testing.T) {
	day1 := time.Date(2026, 10, 28, 23, 59, 0, 0, time.UTC)
	recorder := newTestRecorder(t, &day1)
	signals := func(used string) map[string]string {
		return map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": used}
	}
	recorder.Observe("a", "", "", "claude", signals("0.1"), day1)
	recorder.Flush()
	day2 := day1.Add(2 * time.Minute)
	recorder.Observe("a", "", "", "claude", signals("0.2"), day2)
	recorder.Flush()

	for _, name := range []string{"quota-2026-10-28.jsonl", "quota-2026-10-29.jsonl"} {
		info, err := os.Stat(filepath.Join(recorder.Dir(), name))
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}
	all, _ := recorder.Read(day1.Add(-time.Minute), day2.Add(time.Minute), "claude")
	if len(all) != 2 || all[0].At > all[1].At {
		t.Fatalf("samples = %#v, want both days oldest first", all)
	}
	onlyFirst, _ := recorder.Read(day1.Add(-time.Minute), day1.Add(time.Minute), "claude")
	if len(onlyFirst) != 1 {
		t.Fatalf("samples = %#v, want only the first day", onlyFirst)
	}
}

func TestRecorderPrunesFilesPastRetention(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	recorder := newTestRecorder(t, &now)
	old := filepath.Join(recorder.Dir(), "quota-2026-09-01.jsonl")
	recent := filepath.Join(recorder.Dir(), "quota-2026-10-20.jsonl")
	unrelated := filepath.Join(recorder.Dir(), "notes.txt")
	for _, path := range []string{old, recent, unrelated} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recorder.prune()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old file still present (err=%v)", err)
	}
	for _, path := range []string{recent, unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s removed: %v", path, err)
		}
	}
}

func TestRecorderCloseFlushesPending(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	recorder := newTestRecorder(t, &now)
	recorder.Start()
	recorder.Observe("a", "", "", "claude", map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.5"}, now)
	recorder.Close()
	recorder.Close()
	samples, err := recorder.Read(now.Add(-time.Minute), now.Add(time.Minute), "")
	if err != nil || len(samples) != 1 {
		t.Fatalf("samples = %#v err=%v, want the pending reading flushed on Close", samples, err)
	}
}
