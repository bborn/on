package elastic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bborn/on/internal/inventory"
)

// doFake is a DigitalOcean API with just enough state for the provider: droplets,
// tags and one image. It records every request.
type doFake struct {
	mu       sync.Mutex
	calls    []string
	droplets map[int64]*doDroplet
	tags     map[string]int // tag -> resources carrying it
	nextID   int64
	pollsIP  int // GETs of a new droplet before it has an address
	failSize string
	noIP     bool // a new droplet never gets an address
	paged    bool // serve sizes over two pages
}

func newDOFake() *doFake {
	return &doFake{droplets: map[int64]*doDroplet{}, tags: map[string]int{}, nextID: 100}
}

const doSizesJSON = `{"sizes":[
 {"slug":"s-8vcpu-32gb","memory":32768,"vcpus":8,"disk":640,"price_hourly":0.286,"regions":["nyc3","sfo3","ams3"],"available":true},
 {"slug":"g-8vcpu-32gb","memory":32768,"vcpus":8,"disk":100,"price_hourly":0.375,"regions":["nyc3"],"available":true},
 {"slug":"s-4vcpu-8gb","memory":8192,"vcpus":4,"disk":160,"price_hourly":0.071,"regions":["nyc3","sfo3"],"available":true},
 {"slug":"gpu-h100x1-80gb","memory":245760,"vcpus":20,"disk":720,"price_hourly":0.1,"regions":["nyc3"],"available":true},
 {"slug":"m-4vcpu-32gb","memory":32768,"vcpus":4,"disk":100,"price_hourly":0.25,"regions":["nyc3"],"available":true},
 {"slug":"c-16","memory":32768,"vcpus":16,"disk":200,"price_hourly":0.5,"regions":["nyc3"],"available":false}
]}`

func (f *doFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		return
	}
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	path := r.URL.Path
	switch {
	case r.Method == "GET" && path == "/v2/sizes" && f.paged && r.URL.Query().Get("page") == "":
		// Page one holds only a size too small for the pool; the one that
		// qualifies is on page two, behind an absolute next link.
		io.WriteString(w, `{"sizes":[{"slug":"s-4vcpu-8gb","memory":8192,"vcpus":4,"disk":160,"price_hourly":0.071,"regions":["nyc3"],"available":true}],
		 "links":{"pages":{"next":"https://api.digitalocean.com/v2/sizes?page=2&per_page=200"}}}`)
	case r.Method == "GET" && path == "/v2/sizes":
		io.WriteString(w, doSizesJSON)
	case r.Method == "GET" && path == "/v2/images":
		// 160 GB minimum disk, only in nyc3 and sfo3; one untagged snapshot.
		io.WriteString(w, `{"images":[
		 {"id":9,"name":"on-offerlab-old","created_at":"2026-09-20T10:00:00Z","regions":["nyc3"],"min_disk_size":160,"tags":["on-image:offerlab"]},
		 {"id":11,"name":"on-offerlab-new","created_at":"2026-09-24T10:00:00Z","regions":["nyc3","sfo3"],"min_disk_size":160,"tags":["on-image:offerlab"]},
		 {"id":12,"name":"unrelated","created_at":"2026-09-25T10:00:00Z","regions":["nyc3"],"min_disk_size":20,"tags":[]}]}`)
	case r.Method == "GET" && path == "/v2/account/keys":
		io.WriteString(w, `{"ssh_keys":[{"id":1,"name":"other"},{"id":5,"name":"k"}]}`)
	case r.Method == "POST" && path == "/v2/droplets":
		if body["size"] == f.failSize {
			w.WriteHeader(422)
			io.WriteString(w, `{"id":"unprocessable_entity","message":"Size is not available in this region."}`)
			return
		}
		f.nextID++
		d := &doDroplet{ID: f.nextID, Name: body["name"].(string), Status: "new", SizeSlug: body["size"].(string),
			CreatedAt: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
		d.Region.Slug = body["region"].(string)
		for _, t := range body["tags"].([]any) {
			d.Tags = append(d.Tags, t.(string))
			f.tags[t.(string)]++
		}
		f.droplets[d.ID] = d
		json.NewEncoder(w).Encode(map[string]any{"droplet": d})
	case r.Method == "GET" && path == "/v2/droplets":
		want := r.URL.Query().Get("tag_name")
		var out []*doDroplet
		for _, d := range f.droplets {
			if contains(d.Tags, want) {
				out = append(out, d)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"droplets": out})
	case r.Method == "GET" && strings.HasPrefix(path, "/v2/droplets/"):
		var id int64
		fmt.Sscanf(path, "/v2/droplets/%d", &id)
		d := f.droplets[id]
		if d == nil {
			w.WriteHeader(404)
			return
		}
		if f.pollsIP > 0 {
			f.pollsIP--
		} else if len(d.Networks.V4) == 0 && !f.noIP {
			d.Status = "active"
			d.Networks.V4 = append(d.Networks.V4, struct {
				IP   string `json:"ip_address"`
				Type string `json:"type"`
			}{"10.0.0.1", "private"}, struct {
				IP   string `json:"ip_address"`
				Type string `json:"type"`
			}{"5.5.5.5", "public"})
		}
		json.NewEncoder(w).Encode(map[string]any{"droplet": d})
	case r.Method == "DELETE" && strings.HasPrefix(path, "/v2/droplets/"):
		var id int64
		fmt.Sscanf(path, "/v2/droplets/%d", &id)
		delete(f.droplets, id)
		w.WriteHeader(204)
	case r.Method == "POST" && path == "/v2/tags":
		w.WriteHeader(201)
	case strings.HasSuffix(path, "/resources"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/v2/tags/"), "/resources")
		res := body["resources"].([]any)[0].(map[string]any)
		var id int64
		fmt.Sscanf(res["resource_id"].(string), "%d", &id)
		if d := f.droplets[id]; d != nil {
			if r.Method == "POST" {
				d.Tags = append(d.Tags, name)
				f.tags[name]++
			} else {
				var kept []string
				for _, t := range d.Tags {
					if t != name {
						kept = append(kept, t)
					}
				}
				d.Tags = kept
				f.tags[name]--
			}
		}
		w.WriteHeader(204)
	case r.Method == "GET" && strings.HasPrefix(path, "/v2/tags/"):
		fmt.Fprintf(w, `{"tag":{"resources":{"count":%d}}}`, f.tags[strings.TrimPrefix(path, "/v2/tags/")])
	case r.Method == "DELETE" && strings.HasPrefix(path, "/v2/tags/"):
		delete(f.tags, strings.TrimPrefix(path, "/v2/tags/"))
		w.WriteHeader(204)
	default:
		w.WriteHeader(404)
	}
}

var doCfg = inventory.ProviderConfig{Locations: []string{"sfo3", "nyc3"}, SSHKeys: []string{"k"}}

func newTestDO(t *testing.T, f *doFake) *DigitalOcean {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	d := NewDigitalOcean(pool, doCfg)
	d.BaseURL, d.token = srv.URL, "tok"
	d.Poll = time.Millisecond
	return d
}

func TestDigitalOceanOffersOnlyWhatCanBootTheImage(t *testing.T) {
	d := newTestDO(t, newDOFake())
	m := &Manager{Pool: pool, Providers: []Provider{d}}
	offers, err := m.Offers(false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range offers {
		got = append(got, fmt.Sprintf("%s@%s=%.3f", o.Type, o.Location, o.Cost))
	}
	// Only s-8vcpu-32gb qualifies: g- has too small a disk for the image, m- too
	// few CPUs, c-16 is unavailable, gpu- is never offered, s-4vcpu too small.
	// ams3 is not configured; sfo3 is listed first, so it wins the tie. USD at 0.5.
	if want := "s-8vcpu-32gb@sfo3=0.143 s-8vcpu-32gb@nyc3=0.143"; strings.Join(got, " ") != want {
		t.Fatalf("offers = %v, want %s", got, want)
	}
}

func TestDigitalOceanCreateWaitsForAnAddressAndTags(t *testing.T) {
	f := newDOFake()
	f.pollsIP = 2
	d := newTestDO(t, f)
	now := time.Unix(1790260000, 0)
	s, err := d.Create("on-ol-x", Offer{Provider: "digitalocean", Type: "s-8vcpu-32gb", Location: "nyc3"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.IP != "5.5.5.5" || s.Status != "running" || s.Provider != "digitalocean" || s.Pool != "ol" {
		t.Fatalf("got %+v; want the public address, running, in pool ol", s)
	}
	if !s.LastUsed.Equal(now) {
		t.Fatalf("last used should come from the tag: %v", s.LastUsed)
	}
	d2 := f.droplets[s.ID]
	for _, want := range []string{"on:elastic", "on-pool:ol", "on-last-used:1790260000"} {
		if !contains(d2.Tags, want) {
			t.Errorf("droplet missing tag %q: %v", want, d2.Tags)
		}
	}
	var create string
	for _, c := range f.calls {
		if c == "POST /v2/droplets" {
			create = c
		}
	}
	if create == "" {
		t.Fatal("no create call")
	}
}

func TestDigitalOceanListIsScopedAndTouchReplacesTheTag(t *testing.T) {
	f := newDOFake()
	d := newTestDO(t, f)
	s, err := d.Create("on-ol-x", Offer{Type: "s-8vcpu-32gb", Location: "nyc3"}, time.Unix(1790260000, 0))
	if err != nil {
		t.Fatal(err)
	}
	// A droplet tagged for the pool but not managed by `on` is never listed.
	f.droplets[999] = &doDroplet{ID: 999, Name: "hand-made", Tags: []string{"on-pool:ol"}}

	if err := d.Touch(s, time.Unix(1790263600, 0)); err != nil {
		t.Fatal(err)
	}
	servers, err := d.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "on-ol-x" {
		t.Fatalf("List = %+v, want only on-ol-x", servers)
	}
	if servers[0].LastUsed.Unix() != 1790263600 {
		t.Fatalf("last used = %d, want the touched time", servers[0].LastUsed.Unix())
	}
	if _, left := f.tags["on-last-used:1790260000"]; left {
		t.Fatal("the replaced last-used tag should be deleted once unused")
	}
}

func TestDigitalOceanReadsTheTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "do.env")
	os.WriteFile(path, []byte("# comment\nOTHER=x\nexport DIGITALOCEAN_ACCESS_TOKEN='abc'\n"), 0o600)
	d := NewDigitalOcean(pool, inventory.ProviderConfig{TokenFile: path})
	if tok, err := d.Token(); err != nil || tok != "abc" {
		t.Fatalf("token = %q, %v", tok, err)
	}
}

// stub is a Provider with canned answers, for testing the Manager alone.
type stub struct {
	kind, currency string
	offers         []Offer
	servers        []Server
	listErr        error
	fail           map[string]bool // types whose create fails
	paused         string
	created        []string
}

func (s *stub) Kind() string                                { return s.kind }
func (s *stub) Currency() string                            { return s.currency }
func (s *stub) Offers(bool) ([]Offer, error)                { return s.offers, nil }
func (s *stub) List() ([]Server, error)                     { return s.servers, s.listErr }
func (s *stub) Builders() ([]Server, error)                 { return nil, s.listErr }
func (s *stub) CreateBuilder(string, Offer) (Server, error) { return Server{}, nil }
func (s *stub) SaveImage(Server, time.Time) error           { return nil }
func (s *stub) Touch(Server, time.Time) error               { return nil }
func (s *stub) Delete(Server) error                         { return nil }
func (s *stub) HourlyPrice(Server) float64                  { return 1 }
func (s *stub) PausedDay() string                           { return s.paused }
func (s *stub) Pause(string) error                          { return nil }
func (s *stub) Unpause() error                              { return nil }
func (s *stub) Create(name string, o Offer, _ time.Time) (Server, error) {
	s.created = append(s.created, o.Type)
	if s.fail[o.Type] {
		return Server{}, errors.New("sold out")
	}
	return Server{Provider: s.kind, Name: name, Type: o.Type}, nil
}

func TestManagerPicksTheCheapestAcrossProvidersInOneCurrency(t *testing.T) {
	big := func(p, typ string, price float64) Offer {
		return Offer{Provider: p, Type: typ, Location: "x", CPUs: 16, MemoryGB: 32, Price: price}
	}
	hz := &stub{kind: "hetzner", currency: "EUR", fail: map[string]bool{"cx53": true},
		offers: []Offer{big("hetzner", "cx53", 0.056), big("hetzner", "cpx62", 0.245)}}
	do := &stub{kind: "digitalocean", currency: "USD",
		offers: []Offer{big("digitalocean", "s-8vcpu-32gb", 0.286), {Provider: "digitalocean", Type: "tiny", CPUs: 1, MemoryGB: 1, Price: 0.001}}}
	p := pool
	p.Rates = map[string]float64{"USD": 0.5}
	m := &Manager{Pool: p, Providers: []Provider{hz, do}}

	// In EUR: cx53 0.056, s-8vcpu 0.143, cpx62 0.245. cx53 is sold out, so
	// DigitalOcean's is next; "tiny" is under the pool's minimum.
	s, err := m.Create(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider != "digitalocean" || s.Type != "s-8vcpu-32gb" {
		t.Fatalf("got %s %s, want digitalocean s-8vcpu-32gb", s.Provider, s.Type)
	}
	if len(hz.created) != 1 || len(do.created) != 1 {
		t.Fatalf("attempts: hetzner %v, digitalocean %v", hz.created, do.created)
	}
}

func TestManagerListSurvivesOneProviderFailing(t *testing.T) {
	hz := &stub{kind: "hetzner", currency: "EUR", servers: []Server{{Name: "a"}}}
	do := &stub{kind: "digitalocean", currency: "USD", listErr: errors.New("401")}
	var warned strings.Builder
	m := &Manager{Pool: pool, Providers: []Provider{hz, do}, Warn: &warned}
	servers, err := m.List()
	if err != nil || len(servers) != 1 {
		t.Fatalf("List = %v, %v; want hetzner's server despite digitalocean failing", servers, err)
	}
	if !strings.Contains(warned.String(), "digitalocean: 401") {
		t.Fatalf("the failure should be reported, got %q", warned.String())
	}
	m.Providers = []Provider{do}
	if _, err := m.List(); err == nil {
		t.Fatal("with every provider failing, List must fail")
	}
}

func TestManagerPausedOnAnyProviderRefusesCreate(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	hz := &stub{kind: "hetzner", currency: "EUR"}
	do := &stub{kind: "digitalocean", currency: "USD", paused: "2026-09-24"}
	m := &Manager{Pool: pool, Providers: []Provider{hz, do}}
	if _, err := m.Create(now); !errors.Is(err, ErrPaused) {
		t.Fatalf("want ErrPaused, got %v", err)
	}
}

func TestDigitalOceanDeletesADropletThatNeverGetsAnAddress(t *testing.T) {
	f := newDOFake()
	f.noIP = true
	d := newTestDO(t, f)
	d.Wait = 20 * time.Millisecond
	_, err := d.Create("on-ol-x", Offer{Type: "s-8vcpu-32gb", Location: "nyc3"}, time.Now())
	if err == nil {
		t.Fatal("want an error")
	}
	if len(f.droplets) != 0 {
		t.Fatalf("the droplet must be deleted, not left billing: %d left", len(f.droplets))
	}
}

func TestDigitalOceanFollowsPages(t *testing.T) {
	f := newDOFake()
	f.paged = true
	d := newTestDO(t, f)
	m := &Manager{Pool: pool, Providers: []Provider{d}}
	offers, err := m.Offers(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) == 0 {
		t.Fatalf("the qualifying size is on page two; calls: %v", f.calls)
	}
}

func TestBuilderOffersIgnoreThePoolsTypes(t *testing.T) {
	d := newTestDO(t, newDOFake())
	d.Cfg.Types = []string{"s-8vcpu-32gb"}
	m := &Manager{Pool: pool, Providers: []Provider{d}}
	offers, err := m.Offers(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) == 0 || offers[0].Type != "s-4vcpu-8gb" {
		t.Fatalf("the builder should be the cheapest 4 CPU / 8 GB, not a pinned pool type: %v", offers)
	}
}

func TestManagerCreateUsesAFreshNameEachAttempt(t *testing.T) {
	var names []string
	hz := &naming{stub: stub{kind: "hetzner", currency: "EUR", fail: map[string]bool{"a": true, "b": true},
		offers: []Offer{
			{Provider: "hetzner", Type: "a", CPUs: 16, MemoryGB: 32, Price: 0.1},
			{Provider: "hetzner", Type: "b", CPUs: 16, MemoryGB: 32, Price: 0.2},
			{Provider: "hetzner", Type: "c", CPUs: 16, MemoryGB: 32, Price: 0.3},
		}}, names: &names}
	m := &Manager{Pool: pool, Providers: []Provider{hz}}
	if _, err := m.Create(time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 || names[0] == names[1] || names[1] == names[2] {
		t.Fatalf("each attempt needs its own name, got %v", names)
	}
}

type naming struct {
	stub
	names *[]string
}

func (n *naming) Create(name string, o Offer, now time.Time) (Server, error) {
	*n.names = append(*n.names, name)
	return n.stub.Create(name, o, now)
}

// pausable records Pause calls, and can refuse them.
type pausable struct {
	stub
	refuse bool
	calls  int
}

func (p *pausable) Pause(day string) error {
	p.calls++
	if p.refuse {
		return errors.New("unreachable")
	}
	p.paused = day
	return nil
}

func TestManagerPauseRetriesTheProviderThatMissedIt(t *testing.T) {
	hz := &pausable{stub: stub{kind: "hetzner", currency: "EUR"}}
	do := &pausable{stub: stub{kind: "digitalocean", currency: "USD"}, refuse: true}
	m := &Manager{Pool: pool, Providers: []Provider{hz, do}}
	if err := m.Pause("2026-09-24"); err == nil {
		t.Fatal("a provider that could not be marked must be reported")
	}
	if m.PausedEverywhere("2026-09-24") {
		t.Fatal("not paused everywhere while one provider is unmarked")
	}
	do.refuse = false
	if err := m.Pause("2026-09-24"); err != nil {
		t.Fatal(err)
	}
	if hz.calls != 1 || do.calls != 2 || !m.PausedEverywhere("2026-09-24") {
		t.Fatalf("the retry should mark only the one that missed it: hetzner %d, digitalocean %d", hz.calls, do.calls)
	}
}

func TestManagerRecordsWhichProvidersFailedToList(t *testing.T) {
	hz := &stub{kind: "hetzner", currency: "EUR"}
	do := &stub{kind: "digitalocean", currency: "USD", listErr: errors.New("503")}
	m := &Manager{Pool: pool, Providers: []Provider{hz, do}, Warn: io.Discard}
	if _, err := m.List(); err != nil {
		t.Fatal(err)
	}
	if m.Answered("digitalocean") || !m.Answered("hetzner") {
		t.Fatalf("Failed = %v", m.Failed)
	}
	do.listErr = nil
	m.List()
	if len(m.Failed) != 0 {
		t.Fatalf("a later successful List must clear Failed, got %v", m.Failed)
	}
}
