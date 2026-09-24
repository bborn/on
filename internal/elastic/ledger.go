package elastic

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"time"
)

// Ledger estimates what pools spend, for the daily budget and for anything that
// wants to show the bill. Hetzner has no live per-server billing API, so it is
// counted here: servers are billed per started hour, and each time `on reap`
// sees a server it charges the hours it has started since it was last seen.
//
// It lives on the machine that runs `on reap` on a schedule, because only a
// process that watches continuously can count hours; the pause mark on the
// snapshot is how every other machine learns the budget is spent.
type Ledger struct {
	// Days maps a UTC day to spend per pool.
	Days map[string]map[string]float64 `json:"days"`

	// Billed maps a server name to the hours already charged for it.
	Billed map[string]BilledServer `json:"billed"`
}

// BilledServer is the running tally for one server.
type BilledServer struct {
	Pool     string  `json:"pool"`
	Provider string  `json:"provider,omitempty"`
	Hours    int     `json:"hours"`
	Price    float64 `json:"price"`
	SeenAt   int64   `json:"seen_at"`
}

// LedgerPath honours ON_LEDGER for tests.
func LedgerPath() string {
	if p := os.Getenv("ON_LEDGER"); p != "" {
		return p
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "on", "ledger.json")
}

// LoadLedger reads the ledger, or returns an empty one.
func LoadLedger(path string) (*Ledger, error) {
	l := &Ledger{Days: map[string]map[string]float64{}, Billed: map[string]BilledServer{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, l); err != nil {
		return nil, err
	}
	if l.Days == nil {
		l.Days = map[string]map[string]float64{}
	}
	if l.Billed == nil {
		l.Billed = map[string]BilledServer{}
	}
	return l, nil
}

// Save writes the ledger atomically.
func (l *Ledger) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Charge bills a server's newly started hours to today and returns the amount.
func (l *Ledger) Charge(pool string, s Server, price float64, now time.Time) float64 {
	started := int(math.Ceil(now.Sub(s.Created).Hours()))
	if started < 1 {
		started = 1
	}
	prev := l.Billed[s.Name]
	delta := float64(started-prev.Hours) * price
	if delta < 0 {
		delta = 0
	}
	if l.Days[UTCDay(now)] == nil {
		l.Days[UTCDay(now)] = map[string]float64{}
	}
	l.Days[UTCDay(now)][pool] += delta
	l.Billed[s.Name] = BilledServer{Pool: pool, Provider: s.Provider, Hours: max(started, prev.Hours), Price: price, SeenAt: now.Unix()}
	return delta
}

// Forget drops servers no longer present, so the ledger does not grow forever.
//
// Only servers of providers that answered the listing are forgotten. A server
// missing because its provider could not be reached is still running; dropping
// its tally would bill all its hours again once the provider is back, which
// can push the day over budget and delete every server in the pool.
func (l *Ledger) Forget(pool string, present map[string]bool, answered func(provider string) bool) {
	for name, b := range l.Billed {
		provider := b.Provider
		if provider == "" {
			provider = "hetzner" // tallies from before pools had several providers
		}
		if b.Pool == pool && !present[name] && answered(provider) {
			delete(l.Billed, name)
		}
	}
}

// Spent returns a pool's estimated spend on the given UTC day.
func (l *Ledger) Spent(pool, day string) float64 {
	return l.Days[day][pool]
}
