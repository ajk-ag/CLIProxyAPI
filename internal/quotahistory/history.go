// Package quotahistory keeps a short history of the passive quota readings that
// upstream responses carry (Claude "anthropic-ratelimit-unified-*" and Codex
// "x-codex-*" headers), so the management panel can chart how each account's
// quota moved over time. It never calls an upstream itself: a reading is only
// recorded when real traffic already returned one.
//
// Readings are coalesced to at most one line per account per flush interval and
// only kept when a window changed. They are appended to one JSON-lines file per
// UTC day and files older than the retention period are deleted.
package quotahistory

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// DefaultRetention is how long readings are kept.
	DefaultRetention = 35 * 24 * time.Hour
	// DefaultFlushInterval bounds how often one account can add a line.
	DefaultFlushInterval = 30 * time.Second

	filePrefix = "quota-"
	fileSuffix = ".jsonl"
	dayLayout  = "2006-01-02"
	// maxReadSamples caps one read so a long range cannot build an unbounded response.
	maxReadSamples = 200000
)

// Window is one quota window as reported by the upstream.
type Window struct {
	// ID names the window: "5h", "7d", "7d_sonnet" (Claude) or "primary",
	// "secondary", "<limit>-primary" (Codex).
	ID string `json:"id"`
	// Used is the percentage used, 0-100 (it can exceed 100 when over the limit).
	Used float64 `json:"used"`
	// ResetAt is when the window resets, in Unix seconds (0 when unknown).
	ResetAt int64 `json:"reset_at,omitempty"`
	// Minutes is the window length when the upstream reports it (Codex).
	Minutes int `json:"minutes,omitempty"`
}

// Sample is one recorded reading for one account.
type Sample struct {
	At        int64    `json:"t"`
	Auth      string   `json:"auth"`
	AuthIndex string   `json:"auth_index,omitempty"`
	Name      string   `json:"name,omitempty"`
	Provider  string   `json:"provider"`
	Windows   []Window `json:"windows"`
}

// Recorder collects readings in memory and appends them to daily files.
type Recorder struct {
	dir           string
	retention     time.Duration
	flushInterval time.Duration
	now           func() time.Time

	mu       sync.Mutex
	pending  map[string]Sample
	last     map[string]Sample
	prunedOn string

	writeMu sync.Mutex

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New creates a recorder that stores its files in dir.
func New(dir string) (*Recorder, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("quota history: empty directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("quota history: create %s: %w", dir, err)
	}
	return &Recorder{
		dir:           dir,
		retention:     DefaultRetention,
		flushInterval: DefaultFlushInterval,
		now:           time.Now,
		pending:       make(map[string]Sample),
		last:          make(map[string]Sample),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}, nil
}

// Dir returns the directory holding the history files.
func (r *Recorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

// RetentionDays returns how many days of history are kept.
func (r *Recorder) RetentionDays() int {
	if r == nil {
		return 0
	}
	return int(r.retention / (24 * time.Hour))
}

// Start begins the background flush loop. Call Close to stop it.
func (r *Recorder) Start() {
	if r == nil {
		return
	}
	r.prune()
	go r.loop()
}

// Close flushes pending readings and stops the background loop.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stop)
		<-r.done
	})
}

func (r *Recorder) loop() {
	defer close(r.done)
	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.Flush()
			r.prune()
		case <-r.stop:
			r.Flush()
			return
		}
	}
}

// Observe records the windows found in one passive quota snapshot. It is called on
// the request path, so it only parses and stores the reading in memory.
func (r *Recorder) Observe(authID, authIndex, name, provider string, signals map[string]string, observedAt time.Time) {
	if r == nil || strings.TrimSpace(authID) == "" {
		return
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if observedAt.IsZero() {
		observedAt = r.now()
	}
	windows := ParseWindows(provider, signals, observedAt)
	if len(windows) == 0 {
		return
	}
	sample := Sample{
		At:        observedAt.Unix(),
		Auth:      authID,
		AuthIndex: authIndex,
		Name:      name,
		Provider:  provider,
		Windows:   windows,
	}
	r.mu.Lock()
	r.pending[authID] = sample
	r.mu.Unlock()
}

// Flush appends every pending reading whose windows differ from the last one
// written for that account.
func (r *Recorder) Flush() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return
	}
	toWrite := make([]Sample, 0, len(r.pending))
	for authID, sample := range r.pending {
		if last, ok := r.last[authID]; ok && sameWindows(last.Windows, sample.Windows) {
			continue
		}
		toWrite = append(toWrite, sample)
		r.last[authID] = sample
	}
	r.pending = make(map[string]Sample)
	r.mu.Unlock()
	if len(toWrite) == 0 {
		return
	}
	sort.Slice(toWrite, func(i, j int) bool {
		if toWrite[i].At != toWrite[j].At {
			return toWrite[i].At < toWrite[j].At
		}
		return toWrite[i].Auth < toWrite[j].Auth
	})
	if err := r.append(toWrite); err != nil {
		log.WithError(err).Warn("quota history: append failed")
	}
}

func (r *Recorder) append(samples []Sample) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	byDay := make(map[string][]Sample)
	for _, sample := range samples {
		day := time.Unix(sample.At, 0).UTC().Format(dayLayout)
		byDay[day] = append(byDay[day], sample)
	}
	for day, daySamples := range byDay {
		path := filepath.Join(r.dir, filePrefix+day+fileSuffix)
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		writer := bufio.NewWriter(file)
		for _, sample := range daySamples {
			line, errMarshal := json.Marshal(sample)
			if errMarshal != nil {
				continue
			}
			_, _ = writer.Write(line)
			_ = writer.WriteByte('\n')
		}
		errFlush := writer.Flush()
		errClose := file.Close()
		if errFlush != nil {
			return fmt.Errorf("write %s: %w", path, errFlush)
		}
		if errClose != nil {
			return fmt.Errorf("close %s: %w", path, errClose)
		}
	}
	return nil
}

// Read returns the readings between since and until (inclusive), oldest first.
// An empty provider returns every provider. Pending readings are flushed first.
func (r *Recorder) Read(since, until time.Time, provider string) ([]Sample, error) {
	if r == nil {
		return nil, nil
	}
	r.Flush()
	provider = strings.ToLower(strings.TrimSpace(provider))
	if until.Before(since) {
		since, until = until, since
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	out := make([]Sample, 0)
	sinceUnix, untilUnix := since.Unix(), until.Unix()
	for day := since.UTC().Truncate(24 * time.Hour); !day.After(until.UTC()); day = day.Add(24 * time.Hour) {
		path := filepath.Join(r.dir, filePrefix+day.Format(dayLayout)+fileSuffix)
		file, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			var sample Sample
			if json.Unmarshal(scanner.Bytes(), &sample) != nil {
				continue
			}
			if sample.At < sinceUnix || sample.At > untilUnix {
				continue
			}
			if provider != "" && sample.Provider != provider {
				continue
			}
			out = append(out, sample)
			if len(out) >= maxReadSamples {
				break
			}
		}
		errScan := scanner.Err()
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Debug("quota history: close failed")
		}
		if errScan != nil {
			return nil, fmt.Errorf("read %s: %w", path, errScan)
		}
		if len(out) >= maxReadSamples {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out, nil
}

// prune deletes day files older than the retention period, at most once a day.
func (r *Recorder) prune() {
	now := r.now().UTC()
	today := now.Format(dayLayout)
	r.mu.Lock()
	if r.prunedOn == today {
		r.mu.Unlock()
		return
	}
	r.prunedOn = today
	r.mu.Unlock()

	cutoff := now.Add(-r.retention).Truncate(24 * time.Hour)
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		log.WithError(err).Warn("quota history: list failed")
		return
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		day, errParse := time.Parse(dayLayout, strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix))
		if errParse != nil || !day.Before(cutoff) {
			continue
		}
		if errRemove := os.Remove(filepath.Join(r.dir, name)); errRemove != nil {
			log.WithError(errRemove).Warn("quota history: prune failed")
		}
	}
}

func sameWindows(a, b []Window) bool {
	return reflect.DeepEqual(a, b)
}

var (
	claudeUtilization = regexp.MustCompile(`^anthropic-ratelimit-unified-(.+)-utilization$`)
	codexUsedPercent  = regexp.MustCompile(`^x-codex-(.+)-used-percent$`)
)

// ParseWindows extracts the quota windows from a passive snapshot's signals.
func ParseWindows(provider string, signals map[string]string, observedAt time.Time) []Window {
	if len(signals) == 0 {
		return nil
	}
	lower := make(map[string]string, len(signals))
	for key, value := range signals {
		lower[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	var windows []Window
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		for key, value := range lower {
			match := claudeUtilization.FindStringSubmatch(key)
			if match == nil {
				continue
			}
			fraction, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(fraction) || math.IsInf(fraction, 0) || fraction < 0 {
				continue
			}
			id := match[1]
			window := Window{ID: id, Used: round2(fraction * 100)}
			window.ResetAt = parseUnix(lower["anthropic-ratelimit-unified-"+id+"-reset"])
			windows = append(windows, window)
		}
	case "codex":
		for key, value := range lower {
			match := codexUsedPercent.FindStringSubmatch(key)
			if match == nil {
				continue
			}
			used, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
				continue
			}
			id := match[1]
			minutes, _ := strconv.Atoi(lower["x-codex-"+id+"-window-minutes"])
			resetAt := parseUnix(lower["x-codex-"+id+"-reset-at"])
			if resetAt == 0 {
				if after, errAfter := strconv.ParseInt(lower["x-codex-"+id+"-reset-after-seconds"], 10, 64); errAfter == nil && after > 0 {
					resetAt = observedAt.Unix() + after
				}
			}
			// A window with no length and no reset is a placeholder for a limit the plan lacks.
			if minutes <= 0 && resetAt == 0 {
				continue
			}
			windows = append(windows, Window{ID: id, Used: round2(used), ResetAt: resetAt, Minutes: minutes})
		}
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].ID < windows[j].ID })
	return windows
}

func parseUnix(value string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func round2(value float64) float64 {
	return math.Round(value*100) / 100
}
