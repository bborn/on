package elastic

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bborn/on/internal/inventory"
)

// Provider is one cloud a pool can draw servers from.
//
// Prices a provider reports are in its own currency (Currency); the Manager
// converts them with the pool's rates, so providers never need to know about
// each other.
type Provider interface {
	Kind() string
	Currency() string

	// Offers lists the type/location pairs the provider can boot, with prices.
	// For build=false, those that can boot the pool's newest image; for
	// build=true, those that can boot the plain OS the image is built from.
	Offers(build bool) ([]Offer, error)

	// List returns the pool's servers.
	List() ([]Server, error)

	// Create boots a pool server from the pool's image. It returns once the
	// server has an address; it may not accept ssh yet.
	Create(name string, o Offer, now time.Time) (Server, error)

	// CreateBuilder boots a plain OS server for `on image build`, marked so it is
	// never mistaken for a pool server.
	CreateBuilder(name string, o Offer) (Server, error)

	// SaveImage powers the builder off, snapshots it as the pool's image, and
	// retires older images. It does not delete the builder.
	SaveImage(builder Server, now time.Time) error

	Touch(s Server, now time.Time) error
	Delete(s Server) error

	// HourlyPrice is what the server costs per hour, or 0 when unknown.
	HourlyPrice(s Server) float64

	// PausedDay is the UTC day the daily budget ran out, as marked on the
	// provider's image, or "".
	PausedDay() string
	Pause(day string) error
	Unpause() error
}

// Offer is one type in one location that a provider can boot.
type Offer struct {
	Provider string
	Type     string
	Location string
	CPUs     int
	MemoryGB float64
	DiskGB   int

	// Price is per hour in the provider's currency; Cost is the same in the
	// pool's currency, filled in by the Manager.
	Price float64
	Cost  float64

	// rank is the location's position in the provider's configured list, the
	// tie-breaker between equal costs: a stated preference, not an accident.
	rank int
}

func (o Offer) String() string {
	return fmt.Sprintf("%s %s@%s (%d CPU, %.0f GB)", o.Provider, o.Type, o.Location, o.CPUs, o.MemoryGB)
}

// Builder size: enough to install gems and build assets in reasonable time.
// Kept small because the builder's disk becomes the image's minimum disk.
const (
	builderMinCPUs     = 4
	builderMinMemoryGB = 8
)

// Manager runs one pool across its providers.
type Manager struct {
	Pool      inventory.Pool
	Providers []Provider

	// Warn receives errors from one provider that do not stop the others.
	Warn io.Writer
}

// New returns a manager for the pool with a provider for each configured one.
func New(p inventory.Pool) *Manager {
	m := &Manager{Pool: p, Warn: os.Stderr}
	for _, kind := range p.ProviderNames() {
		cfg := p.Providers[kind]
		switch kind {
		case "hetzner":
			m.Providers = append(m.Providers, NewHetzner(p, cfg))
		case "digitalocean":
			m.Providers = append(m.Providers, NewDigitalOcean(p, cfg))
		}
	}
	return m
}

// Provider returns the named provider, if the pool has it.
func (m *Manager) Provider(kind string) (Provider, bool) {
	for _, pr := range m.Providers {
		if pr.Kind() == kind {
			return pr, true
		}
	}
	return nil, false
}

func (m *Manager) warn(format string, args ...any) {
	if m.Warn != nil {
		fmt.Fprintf(m.Warn, "  "+format+"\n", args...)
	}
}

// List returns the pool's servers across providers, oldest first. One provider
// failing is a warning, so a broken token on one cloud cannot hide, or block
// the reaping of, servers on another. It is an error only when all fail.
func (m *Manager) List() ([]Server, error) {
	var all []Server
	var errs []error
	for _, pr := range m.Providers {
		servers, err := pr.List()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", pr.Kind(), err))
			continue
		}
		all = append(all, servers...)
	}
	if len(errs) > 0 && len(errs) == len(m.Providers) {
		return nil, errors.Join(errs...)
	}
	for _, err := range errs {
		m.warn("%v", err)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Created.Before(all[j].Created) })
	return all, nil
}

// Offers returns what the pool could boot, cheapest first: every provider's
// offers that meet the pool's minimum size (or the builder's, for build),
// priced in the pool's currency.
func (m *Manager) Offers(build bool) ([]Offer, error) {
	minCPUs, minMem := m.Pool.MinCPUs, m.Pool.MinMemoryGB
	if build {
		minCPUs, minMem = builderMinCPUs, builderMinMemoryGB
	}
	var offers []Offer
	var errs []error
	for _, pr := range m.Providers {
		rate, ok := m.Pool.Rate(pr.Currency())
		if !ok {
			errs = append(errs, fmt.Errorf("%s: no rate for %s", pr.Kind(), pr.Currency()))
			continue
		}
		got, err := pr.Offers(build)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", pr.Kind(), err))
			continue
		}
		for _, o := range got {
			if o.CPUs < minCPUs || o.MemoryGB < minMem || o.Price <= 0 {
				continue
			}
			o.Cost = o.Price * rate
			offers = append(offers, o)
		}
	}
	if len(offers) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	for _, err := range errs {
		m.warn("%v", err)
	}
	SortOffers(offers)
	return offers, nil
}

// SortOffers orders offers cheapest first. Equal costs fall back to the
// configured location order, then to names, so the choice is deterministic.
func SortOffers(offers []Offer) {
	sort.SliceStable(offers, func(i, j int) bool {
		a, b := offers[i], offers[j]
		if a.Cost != b.Cost {
			return a.Cost < b.Cost
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Type < b.Type
	})
}

// maxAttempts bounds one Create: past a dozen sold-out answers, the rest are
// unlikely to differ, and each attempt is an API round trip.
const maxAttempts = 12

// Create boots the cheapest server the pool's providers have capacity for.
func (m *Manager) Create(now time.Time) (Server, error) {
	if day := m.PausedDay(); day == UTCDay(now) {
		return Server{}, fmt.Errorf("pool %s: %w for %s (daily_budget %.2f %s) — no new servers until tomorrow UTC",
			m.Pool.Name, ErrPaused, day, m.Pool.DailyBudget, m.Pool.Currency)
	}
	offers, err := m.Offers(false)
	if err != nil {
		return Server{}, err
	}
	if len(offers) == 0 {
		return Server{}, fmt.Errorf("pool %s: no provider offers a server of at least %d CPU / %.0f GB that can boot image %s",
			m.Pool.Name, m.Pool.MinCPUs, m.Pool.MinMemoryGB, m.Pool.Image)
	}
	name := fmt.Sprintf("on-%s-%s", m.Pool.Name, randomSuffix())
	var failures []string
	for i, o := range offers {
		if i == maxAttempts {
			failures = append(failures, fmt.Sprintf("…%d more not tried", len(offers)-i))
			break
		}
		pr, _ := m.Provider(o.Provider)
		s, err := pr.Create(name, o, now)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s %s@%s: %s", o.Provider, o.Type, o.Location, lastLine(err.Error())))
			continue
		}
		return s, nil
	}
	return Server{}, fmt.Errorf("no capacity for pool %s: %s", m.Pool.Name, strings.Join(failures, "; "))
}

// CreateBuilder boots the cheapest plain server of at least builder size on the
// named provider, for `on image build`.
func (m *Manager) CreateBuilder(kind, name string) (Server, Offer, error) {
	pr, ok := m.Provider(kind)
	if !ok {
		return Server{}, Offer{}, fmt.Errorf("pool %s has no provider %q (has: %s)",
			m.Pool.Name, kind, strings.Join(m.Pool.ProviderNames(), ", "))
	}
	offers, err := m.Offers(true)
	if err != nil {
		return Server{}, Offer{}, err
	}
	var failures []string
	for _, o := range offers {
		if o.Provider != kind {
			continue
		}
		if len(failures) == maxAttempts {
			failures = append(failures, "…more not tried")
			break
		}
		s, err := pr.CreateBuilder(name, o)
		if err == nil {
			return s, o, nil
		}
		failures = append(failures, fmt.Sprintf("%s@%s: %s", o.Type, o.Location, lastLine(err.Error())))
	}
	return Server{}, Offer{}, fmt.Errorf("no builder could start on %s: %s", kind, strings.Join(failures, "; "))
}

func (m *Manager) owner(s Server) (Provider, error) {
	if pr, ok := m.Provider(s.Provider); ok {
		return pr, nil
	}
	return nil, fmt.Errorf("%s belongs to provider %q, which pool %s does not have", s.Name, s.Provider, m.Pool.Name)
}

// Touch records a use, which is what keeps `on reap` away.
func (m *Manager) Touch(s Server, now time.Time) error {
	pr, err := m.owner(s)
	if err != nil {
		return err
	}
	return pr.Touch(s, now)
}

// Delete removes the server. Both providers bill a powered-off server, so there
// is no cheaper "stop" to offer.
func (m *Manager) Delete(s Server) error {
	pr, err := m.owner(s)
	if err != nil {
		return err
	}
	return pr.Delete(s)
}

// HourlyPrice is the server's hourly price in the pool's currency, or 0.
func (m *Manager) HourlyPrice(s Server) float64 {
	pr, err := m.owner(s)
	if err != nil {
		return 0
	}
	rate, ok := m.Pool.Rate(pr.Currency())
	if !ok {
		return 0
	}
	return pr.HourlyPrice(s) * rate
}

// PausedDay is the latest pause mark on any provider's image, or "". The mark
// is on every provider's image, so a machine that can reach only one still
// sees it.
func (m *Manager) PausedDay() string {
	latest := ""
	for _, pr := range m.Providers {
		if d := pr.PausedDay(); d > latest {
			latest = d
		}
	}
	return latest
}

// Pause marks every provider's image so no new server starts today.
func (m *Manager) Pause(day string) error {
	var errs []error
	for _, pr := range m.Providers {
		if err := pr.Pause(day); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", pr.Kind(), err))
		}
	}
	return errors.Join(errs...)
}

// Unpause clears pause marks from an earlier day.
func (m *Manager) Unpause() error {
	var errs []error
	for _, pr := range m.Providers {
		if d := pr.PausedDay(); d == "" {
			continue
		}
		if err := pr.Unpause(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", pr.Kind(), err))
		}
	}
	return errors.Join(errs...)
}

// ErrPaused is returned by Create while the daily budget is spent.
var ErrPaused = fmt.Errorf("daily budget reached")
