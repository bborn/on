package elastic

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bborn/on/internal/inventory"
)

var hetznerCfg = inventory.ProviderConfig{Locations: []string{"fsn1", "nbg1"}, SSHKeys: []string{"k"}}

var pool = inventory.Pool{
	Name: "ol", Image: "offerlab", User: "dev", Workdir: "~/worktrees",
	MinCPUs: 8, MinMemoryGB: 30, Currency: "EUR", Rates: map[string]float64{"USD": 0.5},
	Providers:   map[string]inventory.ProviderConfig{"hetzner": hetznerCfg},
	IdleMinutes: 20, MaxHours: 12, MaxServers: 2,
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	fresh := Server{Name: "a", Status: "running", Created: now.Add(-time.Hour), LastUsed: now.Add(-5 * time.Minute)}
	idle := fresh
	idle.LastUsed = now.Add(-30 * time.Minute)
	old := fresh
	old.Created = now.Add(-13 * time.Hour)
	off := fresh
	off.Status = "off"

	cases := []struct {
		name       string
		s          Server
		busy, over bool
		del        bool
	}{
		{"recently used stays", fresh, false, false, false},
		{"idle past the limit goes", idle, false, false, true},
		{"busy protects an idle server", idle, true, false, false},
		{"max_hours beats busy", old, true, false, true},
		{"budget beats busy", fresh, true, true, true},
		{"a stopped server is still billed, so it goes", off, false, false, true},
	}
	for _, c := range cases {
		if got := Decide(pool, c.s, c.busy, c.over, now); got.Delete != c.del {
			t.Errorf("%s: delete=%v (%s), want %v", c.name, got.Delete, got.Reason, c.del)
		}
	}
}

func TestLedgerChargesStartedHoursOnce(t *testing.T) {
	l := &Ledger{Days: map[string]map[string]float64{}, Billed: map[string]BilledServer{}}
	created := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s := Server{Name: "a", Created: created}

	if got := l.Charge("ol", s, 0.5, created.Add(time.Minute)); got != 0.5 {
		t.Fatalf("first sighting should bill the started hour, got %v", got)
	}
	if got := l.Charge("ol", s, 0.5, created.Add(30*time.Minute)); got != 0 {
		t.Fatalf("same hour must not bill twice, got %v", got)
	}
	if got := l.Charge("ol", s, 0.5, created.Add(2*time.Hour+time.Minute)); got != 1.0 {
		t.Fatalf("two more started hours = 1.0, got %v", got)
	}
	if spent := l.Spent("ol", "2026-09-24"); spent != 1.5 {
		t.Fatalf("spent = %v, want 1.5", spent)
	}

	l.Forget("ol", map[string]bool{})
	if _, ok := l.Billed["a"]; ok {
		t.Fatal("a server no longer present should be forgotten")
	}
}

func TestLedgerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Charge("ol", Server{Name: "a", Created: time.Now()}, 0.25, time.Now())
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Spent("ol", UTCDay(time.Now())) != 0.25 {
		t.Fatalf("ledger did not round-trip: %+v", again)
	}
}

// fake records hcloud calls and answers from a script.
type fake struct {
	calls   [][]string
	respond func(args []string) ([]byte, error)
}

func (f *fake) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	return f.respond(args)
}

const snapshotJSON = `[{"id":7,"created":"2026-09-24T10:00:00Z","disk_size":160,"labels":{"on-image":"offerlab"}}]`

// Prices as hcloud lists them. cx53 is cheapest, then cpx62; cx33 is too small
// for the pool, cax41 is Arm, and ccx13's disk is smaller than the snapshot's.
const typesJSON = `[
 {"name":"cpx62","cores":16,"memory":32,"disk":640,"architecture":"x86","deprecated":false,"deprecation":null,
  "prices":[{"location":"fsn1","price_hourly":{"gross":"0.2452"}},{"location":"nbg1","price_hourly":{"gross":"0.2452"}},{"location":"sin","price_hourly":{"gross":"0.3"}}]},
 {"name":"cx53","cores":16,"memory":32,"disk":320,"architecture":"x86","deprecated":false,"deprecation":null,
  "prices":[{"location":"fsn1","price_hourly":{"gross":"0.0561"}},{"location":"nbg1","price_hourly":{"gross":"0.0561"}}]},
 {"name":"cx33","cores":4,"memory":8,"disk":80,"architecture":"x86","deprecated":false,"deprecation":null,
  "prices":[{"location":"fsn1","price_hourly":{"gross":"0.016"}}]},
 {"name":"cax41","cores":16,"memory":32,"disk":320,"architecture":"arm","deprecated":false,"deprecation":null,
  "prices":[{"location":"fsn1","price_hourly":{"gross":"0.03"}}]},
 {"name":"ccx13","cores":16,"memory":32,"disk":80,"architecture":"x86","deprecated":false,"deprecation":null,
  "prices":[{"location":"fsn1","price_hourly":{"gross":"0.02"}}]}
]`

func hetznerFake(create func(joined string) ([]byte, error)) *fake {
	return &fake{respond: func(args []string) ([]byte, error) {
		switch args[0] {
		case "image":
			return []byte(snapshotJSON), nil
		case "server-type":
			return []byte(typesJSON), nil
		case "server":
			return create(strings.Join(args, " "))
		}
		return nil, errors.New("unexpected")
	}}
}

func hetznerManager(f *fake) *Manager {
	h := NewHetzner(pool, hetznerCfg)
	h.Run = f.run
	return &Manager{Pool: pool, Providers: []Provider{h}}
}

func TestHetznerOffersOnlyWhatCanBootTheImage(t *testing.T) {
	m := hetznerManager(hetznerFake(nil))
	offers, err := m.Offers(false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range offers {
		got = append(got, o.Type+"@"+o.Location)
	}
	want := "cx53@fsn1 cx53@nbg1 cpx62@fsn1 cpx62@nbg1"
	if strings.Join(got, " ") != want {
		t.Fatalf("offers = %v, want %s (cheapest first, configured locations only, no Arm, no small disk, none under the minimum)", got, want)
	}
}

func TestCreateFallsBackToTheNextCheapest(t *testing.T) {
	f := hetznerFake(func(joined string) ([]byte, error) {
		if strings.Contains(joined, "--type cpx62 --location nbg1") {
			return []byte(`{"server":{"id":1,"name":"on-ol-x","status":"initializing",
				"public_net":{"ipv4":{"ip":"1.2.3.4"}},"server_type":{"name":"cpx62"},
				"datacenter":{"location":{"name":"nbg1"}},"labels":{"on-pool":"ol"}}}`), nil
		}
		return []byte("resource_unavailable"), errors.New("error during placement (resource_unavailable)")
	})
	s, err := hetznerManager(f).Create(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider != "hetzner" || s.Type != "cpx62" || s.Location != "nbg1" || s.IP != "1.2.3.4" {
		t.Fatalf("got %+v", s)
	}
	var creates []string
	for _, c := range f.calls {
		if c[0] == "server" && c[1] == "create" {
			creates = append(creates, strings.Join(c, " "))
		}
	}
	// cx53@fsn1, cx53@nbg1 and cpx62@fsn1 are sold out before cpx62@nbg1 works.
	if len(creates) != 4 {
		t.Fatalf("expected 4 create attempts, got %d", len(creates))
	}
	for _, want := range []string{"--image 7", "--label on=elastic", "--label on-pool=ol", "--ssh-key k"} {
		if !strings.Contains(creates[3], want) {
			t.Errorf("create call missing %q: %s", want, creates[3])
		}
	}
}

func TestCreateRefusesWhilePausedToday(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	f := &fake{respond: func(args []string) ([]byte, error) {
		return []byte(`[{"id":7,"created":"2026-09-24T10:00:00Z","labels":{"on-image":"offerlab","on-paused":"2026-09-24"}}]`), nil
	}}
	if _, err := hetznerManager(f).Create(now); !errors.Is(err, ErrPaused) {
		t.Fatalf("want ErrPaused, got %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("a paused pool must not attempt a create, made %d calls", len(f.calls))
	}
}

func TestListScopesToThePoolAndParses(t *testing.T) {
	f := &fake{respond: func(args []string) ([]byte, error) {
		return []byte(`[{"id":2,"name":"on-ol-b","status":"running","created":"2026-09-24T11:00:00Z",
			"labels":{"on-pool":"ol","on-last-used":"1790260000"},"public_net":{"ipv4":{"ip":"5.6.7.8"}},
			"server_type":{"name":"cx53"},"datacenter":{"location":{"name":"fsn1"}}},
			{"id":1,"name":"on-ol-a","status":"running","created":"2026-09-24T10:00:00Z",
			"labels":{"on-pool":"ol"},"public_net":{"ipv4":{"ip":"1.2.3.4"}},
			"server_type":{"name":"cx53"},"location":{"name":"nbg1"},"datacenter":null}]`), nil
	}}
	h := NewHetzner(pool, hetznerCfg)
	h.Run = f.run
	servers, err := h.List()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls[0], " "); !strings.Contains(got, "-l on=elastic,on-pool=ol") {
		t.Fatalf("list must be scoped by labels, got: %s", got)
	}
	if servers[0].Name != "on-ol-a" || servers[1].LastUsed.Unix() != 1790260000 {
		t.Fatalf("want oldest first with parsed last-used, got %+v", servers)
	}
	if servers[0].Location != "nbg1" || servers[1].Location != "fsn1" {
		t.Fatalf("location must come from either shape, got %q and %q", servers[0].Location, servers[1].Location)
	}
	if !servers[0].LastUsed.Equal(servers[0].Created) {
		t.Fatal("a server without on-last-used counts as used when created")
	}
}

func TestRenderSSHConfig(t *testing.T) {
	out := RenderSSHConfig(pool, []Server{{Name: "on-ol-a", IP: "1.2.3.4"}, {Name: "on-ol-b"}})
	if !strings.Contains(out, "Host on-ol-a\n  HostName 1.2.3.4\n  HostKeyAlias on-ol-a\n  User dev\n") {
		t.Fatalf("missing host block:\n%s", out)
	}
	if strings.Contains(out, "on-ol-b") {
		t.Fatal("a server without an IP yet should be skipped")
	}
}
