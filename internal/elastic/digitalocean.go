package elastic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bborn/on/internal/inventory"
)

// DigitalOcean is a pool's servers (droplets) on DigitalOcean.
//
// It talks to the REST API directly: doctl adds nothing `on` needs, and one less
// CLI to install keeps a new machine's setup to the `on` binary and a token file.
//
// DigitalOcean has tags, not labels, and a tag is a bare name, so each label
// becomes a "key:value" tag. The last-used time is a tag too, replaced on every
// use; a tag is a global object, so the old one is deleted once nothing else
// carries it.
type DigitalOcean struct {
	Pool inventory.Pool
	Cfg  inventory.ProviderConfig

	// BaseURL and Client are replaced by tests.
	BaseURL string
	Client  *http.Client

	token  string
	sizes  []doSize
	image  *doImage
	keyIDs []int64
}

// doBaseImage is what `on image build` provisions from.
const doBaseImage = "ubuntu-24-04-x64"

// NewDigitalOcean returns the pool's DigitalOcean provider.
func NewDigitalOcean(p inventory.Pool, cfg inventory.ProviderConfig) *DigitalOcean {
	return &DigitalOcean{Pool: p, Cfg: cfg, BaseURL: "https://api.digitalocean.com",
		Client: &http.Client{Timeout: 60 * time.Second}}
}

func (d *DigitalOcean) Kind() string     { return "digitalocean" }
func (d *DigitalOcean) Currency() string { return inventory.ProviderCurrency["digitalocean"] }

func tag(key, value string) string { return key + ":" + value }

// Token reads DIGITALOCEAN_ACCESS_TOKEN from the token file, else the
// environment.
func (d *DigitalOcean) Token() (string, error) {
	if d.token != "" {
		return d.token, nil
	}
	const name = "DIGITALOCEAN_ACCESS_TOKEN"
	if d.Cfg.TokenFile != "" {
		path := expandHome(d.Cfg.TokenFile)
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("token_file: %w", err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export "))
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == name {
				d.token = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
		if d.token == "" {
			return "", fmt.Errorf("%s has no %s", path, name)
		}
		return d.token, nil
	}
	if d.token = os.Getenv(name); d.token == "" {
		return "", fmt.Errorf("no token: set token_file or %s", name)
	}
	return d.token, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// call makes one API request. A non-2xx answer is an error carrying the API's
// own message, which is what says "size unavailable in this region".
func (d *DigitalOcean) call(method, path string, body, out any) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, d.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.Client.Do(req)
	if err != nil {
		return fmt.Errorf("digitalocean %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("digitalocean %s %s: %d %s", method, path, resp.StatusCode, e.Message)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("digitalocean %s %s: parsing response: %w", method, path, err)
		}
	}
	return nil
}

type doDroplet struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	SizeSlug  string    `json:"size_slug"`
	Region    struct {
		Slug string `json:"slug"`
	} `json:"region"`
	Networks struct {
		V4 []struct {
			IP   string `json:"ip_address"`
			Type string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
	Tags []string `json:"tags"`
}

// doStatus maps droplet states onto the ones Decide knows: a droplet that is
// "new" is still booting, and one that is "off" is still billed.
var doStatus = map[string]string{"new": "initializing", "active": "running"}

func (dr doDroplet) server() Server {
	s := Server{
		Provider: "digitalocean",
		ID:       dr.ID,
		Name:     dr.Name,
		Status:   dr.Status,
		Type:     dr.SizeSlug,
		Location: dr.Region.Slug,
		Created:  dr.CreatedAt,
		LastUsed: dr.CreatedAt,
	}
	if st, ok := doStatus[dr.Status]; ok {
		s.Status = st
	}
	for _, n := range dr.Networks.V4 {
		if n.Type == "public" {
			s.IP = n.IP
		}
	}
	for _, t := range dr.Tags {
		if v, ok := strings.CutPrefix(t, LabelPool+":"); ok {
			s.Pool = v
		}
		if v, ok := strings.CutPrefix(t, LabelLastUsed+":"); ok {
			if sec, err := strconv.ParseInt(v, 10, 64); err == nil && time.Unix(sec, 0).After(s.LastUsed) {
				s.LastUsed = time.Unix(sec, 0)
			}
		}
	}
	return s
}

// List returns the pool's droplets, oldest first.
func (d *DigitalOcean) List() ([]Server, error) {
	var resp struct {
		Droplets []doDroplet `json:"droplets"`
	}
	q := url.Values{"tag_name": {tag(LabelPool, d.Pool.Name)}, "per_page": {"200"}}
	if err := d.call("GET", "/v2/droplets?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	var servers []Server
	for _, dr := range resp.Droplets {
		if contains(dr.Tags, tag(LabelManaged, managedValue)) {
			servers = append(servers, dr.server())
		}
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Created.Before(servers[j].Created) })
	return servers, nil
}

type doSize struct {
	Slug        string   `json:"slug"`
	Memory      int      `json:"memory"`
	VCPUs       int      `json:"vcpus"`
	Disk        int      `json:"disk"`
	PriceHourly float64  `json:"price_hourly"`
	Regions     []string `json:"regions"`
	Available   bool     `json:"available"`
}

func (d *DigitalOcean) allSizes() ([]doSize, error) {
	if d.sizes != nil {
		return d.sizes, nil
	}
	var resp struct {
		Sizes []doSize `json:"sizes"`
	}
	if err := d.call("GET", "/v2/sizes?per_page=200", nil, &resp); err != nil {
		return nil, err
	}
	d.sizes = resp.Sizes
	return d.sizes, nil
}

type doImage struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
	Regions     []string  `json:"regions"`
	MinDiskSize int       `json:"min_disk_size"`
	Tags        []string  `json:"tags"`
}

// images lists the pool image's snapshots, newest first.
func (d *DigitalOcean) images() ([]doImage, error) {
	var resp struct {
		Images []doImage `json:"images"`
	}
	if err := d.call("GET", "/v2/images?private=true&per_page=200", nil, &resp); err != nil {
		return nil, err
	}
	var out []doImage
	for _, im := range resp.Images {
		if contains(im.Tags, tag(LabelImage, d.Pool.Image)) {
			out = append(out, im)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (d *DigitalOcean) poolImage() (doImage, error) {
	if d.image != nil {
		return *d.image, nil
	}
	ims, err := d.images()
	if err != nil {
		return doImage{}, err
	}
	if len(ims) == 0 {
		return doImage{}, fmt.Errorf("no snapshot tagged %s — `on image build %s digitalocean` makes one",
			tag(LabelImage, d.Pool.Image), d.Pool.Name)
	}
	d.image = &ims[0]
	return ims[0], nil
}

// Offers lists each size in each configured region it can boot in. Pool servers
// also need the image to be in that region (a snapshot lives where it was taken
// until it is transferred) and a disk at least the image's minimum.
func (d *DigitalOcean) Offers(build bool) ([]Offer, error) {
	var regions []string
	minDisk := 0
	if build {
		regions = d.Cfg.Locations
	} else {
		im, err := d.poolImage()
		if err != nil {
			return nil, err
		}
		regions, minDisk = im.Regions, im.MinDiskSize
	}
	sizes, err := d.allSizes()
	if err != nil {
		return nil, err
	}
	var offers []Offer
	for _, sz := range sizes {
		// GPU droplets are priced for their GPUs; never the cheapest for this.
		if !sz.Available || sz.Disk < minDisk || strings.HasPrefix(sz.Slug, "gpu-") {
			continue
		}
		if len(d.Cfg.Types) > 0 && !contains(d.Cfg.Types, sz.Slug) {
			continue
		}
		for _, r := range sz.Regions {
			rank := indexOf(d.Cfg.Locations, r)
			if rank < 0 || !contains(regions, r) {
				continue
			}
			offers = append(offers, Offer{Provider: "digitalocean", Type: sz.Slug, Location: r,
				CPUs: sz.VCPUs, MemoryGB: float64(sz.Memory) / 1024, DiskGB: sz.Disk, Price: sz.PriceHourly, rank: rank})
		}
	}
	return offers, nil
}

// sshKeyIDs resolves the configured key names to the account's key IDs.
func (d *DigitalOcean) sshKeyIDs() ([]int64, error) {
	if d.keyIDs != nil || len(d.Cfg.SSHKeys) == 0 {
		return d.keyIDs, nil
	}
	var resp struct {
		Keys []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"ssh_keys"`
	}
	if err := d.call("GET", "/v2/account/keys?per_page=200", nil, &resp); err != nil {
		return nil, err
	}
	var ids []int64
	for _, want := range d.Cfg.SSHKeys {
		found := false
		for _, k := range resp.Keys {
			if k.Name == want {
				ids, found = append(ids, k.ID), true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("no ssh key named %q on the DigitalOcean account", want)
		}
	}
	d.keyIDs = ids
	return ids, nil
}

// Create boots a pool droplet from the newest image.
func (d *DigitalOcean) Create(name string, o Offer, now time.Time) (Server, error) {
	im, err := d.poolImage()
	if err != nil {
		return Server{}, err
	}
	return d.create(name, o, im.ID, []string{
		tag(LabelManaged, managedValue),
		tag(LabelPool, d.Pool.Name),
		tag(LabelLastUsed, strconv.FormatInt(now.Unix(), 10)),
	})
}

// CreateBuilder boots plain Ubuntu, tagged on:builder so no pool lists it.
func (d *DigitalOcean) CreateBuilder(name string, o Offer) (Server, error) {
	return d.create(name, o, doBaseImage, []string{tag(LabelManaged, "builder")})
}

// dropletPoll is how often and how long create waits for the droplet's address.
var dropletPoll, dropletWait = 3 * time.Second, 3 * time.Minute

// create boots a droplet and waits for its public address: DigitalOcean
// assigns it after the create call returns, and `on` reaches servers by address.
func (d *DigitalOcean) create(name string, o Offer, image any, tags []string) (Server, error) {
	keys, err := d.sshKeyIDs()
	if err != nil {
		return Server{}, err
	}
	body := map[string]any{"name": name, "region": o.Location, "size": o.Type, "image": image,
		"ssh_keys": keys, "tags": tags, "ipv6": false, "monitoring": false}
	var created struct {
		Droplet doDroplet `json:"droplet"`
	}
	if err := d.call("POST", "/v2/droplets", body, &created); err != nil {
		return Server{}, err
	}
	deadline := time.Now().Add(dropletWait)
	for {
		var got struct {
			Droplet doDroplet `json:"droplet"`
		}
		err := d.call("GET", fmt.Sprintf("/v2/droplets/%d", created.Droplet.ID), nil, &got)
		if err == nil {
			if s := got.Droplet.server(); s.IP != "" {
				return s, nil
			}
		}
		if time.Now().After(deadline) {
			return Server{}, fmt.Errorf("droplet %s (%d) has no address after %s — delete it with `on down %s`",
				name, created.Droplet.ID, dropletWait, name)
		}
		time.Sleep(dropletPoll)
	}
}

type doAction struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

// action starts a droplet action and waits for it to finish.
func (d *DigitalOcean) action(dropletID int64, body map[string]any, wait time.Duration) error {
	var resp struct {
		Action doAction `json:"action"`
	}
	if err := d.call("POST", fmt.Sprintf("/v2/droplets/%d/actions", dropletID), body, &resp); err != nil {
		return err
	}
	return d.waitAction(resp.Action, wait)
}

func (d *DigitalOcean) waitAction(a doAction, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for a.Status != "completed" {
		if a.Status == "errored" {
			return fmt.Errorf("digitalocean action %d errored", a.ID)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("digitalocean action %d still %s after %s", a.ID, a.Status, wait)
		}
		time.Sleep(dropletPoll)
		var resp struct {
			Action doAction `json:"action"`
		}
		if err := d.call("GET", fmt.Sprintf("/v2/actions/%d", a.ID), nil, &resp); err != nil {
			return err
		}
		a = resp.Action
	}
	return nil
}

// SaveImage powers the builder off, snapshots it, tags the snapshot as the pool's
// image, starts copying it to the other configured regions (droplets can only
// boot from a snapshot in their own region), and deletes older snapshots.
func (d *DigitalOcean) SaveImage(b Server, now time.Time) error {
	old, err := d.images()
	if err != nil {
		return err
	}
	if err := d.action(b.ID, map[string]any{"type": "power_off"}, 5*time.Minute); err != nil {
		return err
	}
	name := fmt.Sprintf("on-%s-%s", d.Pool.Image, now.UTC().Format("20060102-1504"))
	if err := d.action(b.ID, map[string]any{"type": "snapshot", "name": name}, 90*time.Minute); err != nil {
		return err
	}
	var snaps struct {
		Snapshots []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"snapshots"`
	}
	if err := d.call("GET", fmt.Sprintf("/v2/droplets/%d/snapshots?per_page=200", b.ID), nil, &snaps); err != nil {
		return err
	}
	var id int64
	for _, s := range snaps.Snapshots {
		if s.Name == name {
			id = s.ID
		}
	}
	if id == 0 {
		return fmt.Errorf("snapshot %s not found on droplet %s", name, b.Name)
	}
	if err := d.tagResource(tag(LabelImage, d.Pool.Image), "image", id); err != nil {
		return err
	}
	d.image = nil
	for _, r := range d.Cfg.Locations {
		if r == b.Location {
			continue
		}
		if err := d.call("POST", fmt.Sprintf("/v2/images/%d/actions", id),
			map[string]any{"type": "transfer", "region": r}, nil); err != nil {
			return fmt.Errorf("image saved, but copying it to %s failed: %w", r, err)
		}
	}
	for _, im := range old {
		if err := d.call("DELETE", fmt.Sprintf("/v2/images/%d", im.ID), nil, nil); err != nil {
			return fmt.Errorf("new image saved, but deleting old snapshot %d failed: %w", im.ID, err)
		}
	}
	return nil
}

func (d *DigitalOcean) tagResource(name, kind string, id int64) error {
	if err := d.call("POST", "/v2/tags", map[string]any{"name": name}, nil); err != nil && !strings.Contains(err.Error(), " 422 ") {
		return err
	}
	return d.call("POST", "/v2/tags/"+url.PathEscape(name)+"/resources", map[string]any{
		"resources": []map[string]string{{"resource_id": strconv.FormatInt(id, 10), "resource_type": kind}}}, nil)
}

func (d *DigitalOcean) untagResource(name, kind string, id int64) error {
	return d.call("DELETE", "/v2/tags/"+url.PathEscape(name)+"/resources", map[string]any{
		"resources": []map[string]string{{"resource_id": strconv.FormatInt(id, 10), "resource_type": kind}}}, nil)
}

// deleteTagIfUnused removes a tag nothing carries any more, so replaced
// last-used and pause tags do not pile up on the account.
func (d *DigitalOcean) deleteTagIfUnused(name string) {
	var resp struct {
		Tag struct {
			Resources struct {
				Count int `json:"count"`
			} `json:"resources"`
		} `json:"tag"`
	}
	if d.call("GET", "/v2/tags/"+url.PathEscape(name), nil, &resp) == nil && resp.Tag.Resources.Count == 0 {
		_ = d.call("DELETE", "/v2/tags/"+url.PathEscape(name), nil, nil)
	}
}

// Touch replaces the droplet's last-used tag.
func (d *DigitalOcean) Touch(s Server, now time.Time) error {
	var got struct {
		Droplet doDroplet `json:"droplet"`
	}
	if err := d.call("GET", fmt.Sprintf("/v2/droplets/%d", s.ID), nil, &got); err != nil {
		return err
	}
	fresh := tag(LabelLastUsed, strconv.FormatInt(now.Unix(), 10))
	if err := d.tagResource(fresh, "droplet", s.ID); err != nil {
		return err
	}
	for _, t := range got.Droplet.Tags {
		if strings.HasPrefix(t, LabelLastUsed+":") && t != fresh {
			if err := d.untagResource(t, "droplet", s.ID); err == nil {
				d.deleteTagIfUnused(t)
			}
		}
	}
	return nil
}

// Delete removes the droplet.
func (d *DigitalOcean) Delete(s Server) error {
	return d.call("DELETE", fmt.Sprintf("/v2/droplets/%d", s.ID), nil, nil)
}

// HourlyPrice is the size's hourly price, or 0 when unknown.
func (d *DigitalOcean) HourlyPrice(s Server) float64 {
	sizes, err := d.allSizes()
	if err != nil {
		return 0
	}
	for _, sz := range sizes {
		if sz.Slug == s.Type {
			return sz.PriceHourly
		}
	}
	return 0
}

// PausedDay reads the pause tag on the pool image.
func (d *DigitalOcean) PausedDay() string {
	im, err := d.poolImage()
	if err != nil {
		return ""
	}
	day := ""
	for _, t := range im.Tags {
		if v, ok := strings.CutPrefix(t, LabelPaused+":"); ok && v > day {
			day = v
		}
	}
	return day
}

// Pause tags the pool image so no new droplet starts today.
func (d *DigitalOcean) Pause(day string) error {
	im, err := d.poolImage()
	if err != nil {
		return err
	}
	d.image = nil
	return d.tagResource(tag(LabelPaused, day), "image", im.ID)
}

// Unpause removes pause tags from the pool image.
func (d *DigitalOcean) Unpause() error {
	im, err := d.poolImage()
	if err != nil {
		return err
	}
	d.image = nil
	for _, t := range im.Tags {
		if strings.HasPrefix(t, LabelPaused+":") {
			if err := d.untagResource(t, "image", im.ID); err != nil {
				return err
			}
			d.deleteTagIfUnused(t)
		}
	}
	return nil
}
