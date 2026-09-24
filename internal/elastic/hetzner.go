package elastic

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bborn/on/internal/inventory"
)

// Hetzner is a pool's servers on Hetzner Cloud.
//
// It drives the hcloud CLI rather than the API, so the token stays in an hcloud
// context, and whatever `on` does can be inspected or undone by hand with the
// same tool.
type Hetzner struct {
	Pool inventory.Pool
	Cfg  inventory.ProviderConfig

	// Run executes hcloud with the given arguments. Tests replace it.
	Run func(args ...string) ([]byte, error)

	types []hcloudType
	snap  *Snapshot
}

// hetznerBaseImage is what `on image build` provisions from.
const hetznerBaseImage = "ubuntu-24.04"

// NewHetzner returns the pool's Hetzner provider, shelling out to hcloud.
func NewHetzner(p inventory.Pool, cfg inventory.ProviderConfig) *Hetzner {
	h := &Hetzner{Pool: p, Cfg: cfg}
	h.Run = func(args ...string) ([]byte, error) {
		full := append([]string{}, args...)
		if cfg.Context != "" {
			full = append([]string{"--context", cfg.Context}, full...)
		}
		// stdout only: hcloud writes "Waiting for …" progress to stderr, which
		// would corrupt the JSON. stderr is kept for the error message.
		var stderr strings.Builder
		cmd := exec.Command("hcloud", full...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			// The verb, not the whole command line: these land in one-line
			// summaries of every offer tried, where hcloud's message is the news.
			verb := strings.Join(args[:min(2, len(args))], " ")
			msg := strings.TrimPrefix(strings.TrimSpace(stderr.String()), "hcloud: ")
			if msg == "" {
				msg = err.Error()
			}
			return out, fmt.Errorf("hcloud %s: %s", verb, msg)
		}
		return out, nil
	}
	return h
}

func (h *Hetzner) Kind() string     { return "hetzner" }
func (h *Hetzner) Currency() string { return inventory.ProviderCurrency["hetzner"] }

type apiServer struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Created   time.Time         `json:"created"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
	ServerType struct {
		Name string `json:"name"`
	} `json:"server_type"`
	// Newer hcloud releases put the location at the top level and leave
	// datacenter null; older ones only nest it under datacenter. Read both.
	Location *struct {
		Name string `json:"name"`
	} `json:"location"`
	Datacenter *struct {
		Location struct {
			Name string `json:"name"`
		} `json:"location"`
	} `json:"datacenter"`
}

func (a apiServer) location() string {
	if a.Location != nil && a.Location.Name != "" {
		return a.Location.Name
	}
	if a.Datacenter != nil {
		return a.Datacenter.Location.Name
	}
	return ""
}

func (a apiServer) server() Server {
	s := Server{
		Provider: "hetzner",
		ID:       a.ID,
		Name:     a.Name,
		IP:       a.PublicNet.IPv4.IP,
		Status:   a.Status,
		Type:     a.ServerType.Name,
		Location: a.location(),
		Pool:     a.Labels[LabelPool],
		Created:  a.Created,
	}
	if sec, err := strconv.ParseInt(a.Labels[LabelLastUsed], 10, 64); err == nil {
		s.LastUsed = time.Unix(sec, 0)
	} else {
		s.LastUsed = a.Created
	}
	return s
}

// List returns the pool's servers, oldest first.
func (h *Hetzner) List() ([]Server, error) { return h.list(managedValue) }

// Builders returns the pool's image builders.
func (h *Hetzner) Builders() ([]Server, error) { return h.list(builderValue) }

func (h *Hetzner) list(managed string) ([]Server, error) {
	out, err := h.Run("server", "list", "-o", "json",
		"-l", fmt.Sprintf("%s=%s,%s=%s", LabelManaged, managed, LabelPool, h.Pool.Name))
	if err != nil {
		return nil, err
	}
	var raw []apiServer
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing hcloud server list: %w", err)
	}
	servers := make([]Server, 0, len(raw))
	for _, r := range raw {
		servers = append(servers, r.server())
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Created.Before(servers[j].Created) })
	return servers, nil
}

// Snapshot is the image a pool boots from.
type Snapshot struct {
	ID       int64
	Created  time.Time
	Labels   map[string]string
	DiskSize float64
}

// Snapshot returns the newest snapshot labelled for this pool's image.
func (h *Hetzner) Snapshot() (Snapshot, error) {
	if h.snap != nil {
		return *h.snap, nil
	}
	snaps, err := h.snapshots()
	if err != nil {
		return Snapshot{}, err
	}
	if len(snaps) == 0 {
		return Snapshot{}, fmt.Errorf("no snapshot labelled %s=%s in context %q — `on image build %s hetzner` makes one",
			LabelImage, h.Pool.Image, h.Cfg.Context, h.Pool.Name)
	}
	h.snap = &snaps[0]
	return snaps[0], nil
}

// snapshots lists the pool image's snapshots, newest first.
func (h *Hetzner) snapshots() ([]Snapshot, error) {
	out, err := h.Run("image", "list", "-t", "snapshot", "-o", "json",
		"-l", fmt.Sprintf("%s=%s", LabelImage, h.Pool.Image))
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID       int64             `json:"id"`
		Created  time.Time         `json:"created"`
		Labels   map[string]string `json:"labels"`
		DiskSize float64           `json:"disk_size"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing hcloud image list: %w", err)
	}
	snaps := make([]Snapshot, 0, len(raw))
	for _, r := range raw {
		snaps = append(snaps, Snapshot{ID: r.ID, Created: r.Created, Labels: r.Labels, DiskSize: r.DiskSize})
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Created.After(snaps[j].Created) })
	return snaps, nil
}

type hcloudType struct {
	Name         string  `json:"name"`
	Cores        int     `json:"cores"`
	Memory       float64 `json:"memory"`
	Disk         int     `json:"disk"`
	Architecture string  `json:"architecture"`
	Deprecated   bool    `json:"deprecated"`
	Deprecation  *struct {
		Announced string `json:"announced"`
	} `json:"deprecation"`
	Prices []struct {
		Location    string `json:"location"`
		PriceHourly struct {
			Gross string `json:"gross"`
		} `json:"price_hourly"`
	} `json:"prices"`
}

func (h *Hetzner) serverTypes() ([]hcloudType, error) {
	if h.types != nil {
		return h.types, nil
	}
	out, err := h.Run("server-type", "list", "-o", "json")
	if err != nil {
		return nil, err
	}
	var types []hcloudType
	if err := json.Unmarshal(out, &types); err != nil {
		return nil, fmt.Errorf("parsing hcloud server-type list: %w", err)
	}
	h.types = types
	return types, nil
}

// Offers lists each x86 type in each configured location with a price. Pool
// servers need a disk at least as large as the snapshot's, which Hetzner
// enforces; the image is x86, so Arm types are left out.
func (h *Hetzner) Offers(build bool) ([]Offer, error) {
	minDisk := 0.0
	if !build {
		snap, err := h.Snapshot()
		if err != nil {
			return nil, err
		}
		minDisk = snap.DiskSize
	}
	types, err := h.serverTypes()
	if err != nil {
		return nil, err
	}
	var offers []Offer
	for _, t := range types {
		if t.Architecture != "x86" || t.Deprecated || t.Deprecation != nil || float64(t.Disk) < minDisk {
			continue
		}
		// types: limits pool servers; a builder is not one, and a big pinned
		// type would make a big image that smaller types cannot boot.
		if !build && len(h.Cfg.Types) > 0 && !contains(h.Cfg.Types, t.Name) {
			continue
		}
		for _, p := range t.Prices {
			rank := indexOf(h.Cfg.Locations, p.Location)
			price, err := strconv.ParseFloat(p.PriceHourly.Gross, 64)
			if rank < 0 || err != nil {
				continue
			}
			offers = append(offers, Offer{Provider: "hetzner", Type: t.Name, Location: p.Location,
				CPUs: t.Cores, MemoryGB: t.Memory, DiskGB: t.Disk, Price: price, rank: rank})
		}
	}
	return offers, nil
}

// Create boots a pool server from the newest snapshot.
func (h *Hetzner) Create(name string, o Offer, now time.Time) (Server, error) {
	snap, err := h.Snapshot()
	if err != nil {
		return Server{}, err
	}
	return h.create(name, o, strconv.FormatInt(snap.ID, 10), []string{
		fmt.Sprintf("%s=%s", LabelManaged, managedValue),
		fmt.Sprintf("%s=%s", LabelPool, h.Pool.Name),
		fmt.Sprintf("%s=%d", LabelLastUsed, now.Unix()),
	})
}

// CreateBuilder boots plain Ubuntu, labelled on=builder so it is never taken for
// a pool server, and with the pool so `on reap` and `on down` can find it.
func (h *Hetzner) CreateBuilder(name string, o Offer) (Server, error) {
	return h.create(name, o, hetznerBaseImage, []string{
		fmt.Sprintf("%s=%s", LabelManaged, builderValue),
		fmt.Sprintf("%s=%s", LabelPool, h.Pool.Name),
	})
}

func (h *Hetzner) create(name string, o Offer, image string, labels []string) (Server, error) {
	args := []string{"server", "create", "-o", "json",
		"--name", name, "--type", o.Type, "--location", o.Location, "--image", image}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	for _, k := range h.Cfg.SSHKeys {
		args = append(args, "--ssh-key", k)
	}
	out, err := h.Run(args...)
	if err != nil {
		return Server{}, err
	}
	var created struct {
		Server apiServer `json:"server"`
	}
	if err := json.Unmarshal(out, &created); err != nil {
		return Server{}, fmt.Errorf("parsing hcloud server create: %w", err)
	}
	return created.Server.server(), nil
}

// SaveImage shuts the builder down, snapshots it with the pool's image label,
// and deletes the older snapshots. hcloud waits for each action to finish.
func (h *Hetzner) SaveImage(b Server, now time.Time) error {
	old, err := h.snapshots()
	if err != nil {
		return err
	}
	if _, err := h.Run("server", "shutdown", b.Name); err != nil {
		return err
	}
	if _, err := h.Run("server", "create-image", "--type", "snapshot",
		"--description", fmt.Sprintf("on %s %s", h.Pool.Image, now.UTC().Format("2006-01-02")),
		"--label", fmt.Sprintf("%s=%s", LabelImage, h.Pool.Image), b.Name); err != nil {
		return err
	}
	h.snap = nil
	// A rebuild during a budget pause must not lift it: the mark lives on the
	// newest image, which is now this one.
	if len(old) > 0 && old[0].Labels[LabelPaused] != "" {
		if err := h.Pause(old[0].Labels[LabelPaused]); err != nil {
			return fmt.Errorf("new image saved, but carrying the pause mark over failed: %w", err)
		}
	}
	for _, s := range old {
		if _, err := h.Run("image", "delete", strconv.FormatInt(s.ID, 10)); err != nil {
			return fmt.Errorf("new image saved, but deleting old snapshot %d failed: %w", s.ID, err)
		}
	}
	return nil
}

// Touch records a use, which is what keeps `on reap` away.
func (h *Hetzner) Touch(s Server, now time.Time) error {
	_, err := h.Run("server", "add-label", "--overwrite", s.Name,
		fmt.Sprintf("%s=%d", LabelLastUsed, now.Unix()))
	return err
}

// Delete removes the server.
func (h *Hetzner) Delete(s Server) error {
	_, err := h.Run("server", "delete", s.Name)
	return err
}

// HourlyPrice is the gross hourly price for the server's type and location, or 0
// when hcloud does not list one.
func (h *Hetzner) HourlyPrice(s Server) float64 {
	types, err := h.serverTypes()
	if err != nil {
		return 0
	}
	for _, t := range types {
		if t.Name != s.Type {
			continue
		}
		for _, p := range t.Prices {
			if p.Location == s.Location {
				v, _ := strconv.ParseFloat(p.PriceHourly.Gross, 64)
				return v
			}
		}
	}
	return 0
}

// PausedDay reads the pause mark from the snapshot, so every machine running
// `on` sees it, not only the one that ran `on reap`.
func (h *Hetzner) PausedDay() string {
	snap, err := h.Snapshot()
	if err != nil {
		return ""
	}
	return snap.Labels[LabelPaused]
}

// Pause marks the snapshot so no new server starts today.
func (h *Hetzner) Pause(day string) error {
	snap, err := h.Snapshot()
	if err != nil {
		return err
	}
	h.snap = nil
	_, err = h.Run("image", "add-label", "--overwrite", strconv.FormatInt(snap.ID, 10),
		fmt.Sprintf("%s=%s", LabelPaused, day))
	return err
}

// Unpause clears a pause mark from an earlier day.
func (h *Hetzner) Unpause() error {
	snap, err := h.Snapshot()
	if err != nil {
		return err
	}
	h.snap = nil
	_, err = h.Run("image", "remove-label", strconv.FormatInt(snap.ID, 10), LabelPaused)
	return err
}

func contains(list []string, s string) bool { return indexOf(list, s) >= 0 }

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}
