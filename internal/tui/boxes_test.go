package tui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/relayclient"
)

// seedConfig points HOME at a temp dir and writes cf there, so config
// Load/Save in the view hit an isolated file.
func seedConfig(t *testing.T, cf config.ClientFile) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveClientFile(cf); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

// fakeDialer returns a Dialer that always yields the given result.
func fakeDialer(c API, addr string, remote bool, err error) Dialer {
	return func(config.Box) (API, string, bool, error) { return c, addr, remote, err }
}

// newRelayBoxesView uses the same root wiring as the product: the root injects
// its relay factory when it pushes the boxes view.
func newRelayBoxesView(t *testing.T, dial Dialer, relay RelayDialer) boxesView {
	t.Helper()
	m := NewModel("local", "", false, fakeAPI{}).WithDialer(dial).WithRelay(relay)
	_, cmd := m.Update(keyRunes('t'))
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("t should push boxesView, got %T", cmd())
	}
	return push.view.(boxesView)
}

func agentsServer(t *testing.T, agents []relayclient.Agent) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/agents" {
			t.Errorf("agents request = %s %s, want GET /agents", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cred-xyz" {
			t.Errorf("agents authorization = %q, want bearer credential", got)
		}
		_ = json.NewEncoder(w).Encode(struct {
			Agents []relayclient.Agent `json:"agents"`
		}{Agents: agents})
	}))
}

func TestBoxesViewLoadsFromConfig(t *testing.T) {
	// refresh reads the seeded config off disk and yields boxesLoadedMsg.
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "pi4", Addr: "192.168.1.6:8088"}, {Name: "blog", Addr: "192.168.1.9:8088"}},
		Current: "pi4",
	})
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	msg := v.refresh(fakeAPI{})()
	loaded, ok := msg.(boxesLoadedMsg)
	if !ok {
		t.Fatalf("refresh should yield boxesLoadedMsg, got %T", msg)
	}
	if len(loaded.boxes) != 2 || loaded.current != "pi4" {
		t.Fatalf("config not loaded: %+v current=%q", loaded.boxes, loaded.current)
	}
}

func TestBoxesViewListsBoxesAndMarksCurrent(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes:   []config.Box{{Name: "pi4", Addr: "192.168.1.6:8088"}, {Name: "blog", Addr: "192.168.1.9:8088"}},
		current: "pi4",
	})
	out := vv.(boxesView).View()
	for _, want := range []string{"pi4", "192.168.1.6:8088", "blog", "current"} {
		if !strings.Contains(out, want) {
			t.Fatalf("boxes view missing %q:\n%s", want, out)
		}
	}
}

func TestTPushesBoxesView(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	_, cmd := m.Update(keyRunes('t'))
	if cmd == nil {
		t.Fatal("t should push a boxes view")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if _, ok := push.view.(boxesView); !ok {
		t.Fatalf("want boxesView pushed, got %T", push.view)
	}
}

func TestTDoesNotStackBoxes(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	depth := len(m.stack)
	_, cmd := m.Update(keyRunes('t'))
	if cmd != nil {
		if _, ok := cmd().(pushMsg); ok {
			t.Fatal("t on the boxes view must not push a second boxes view")
		}
	}
	if len(m.stack) != depth {
		t.Fatalf("stack depth changed: %d -> %d", depth, len(m.stack))
	}
}

func TestEnterOnBoxEmitsSwitch(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{boxes: []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}}, current: "pi4"})
	v = vv.(boxesView)
	// cursor starts at 0 (pi4, current); move to blog and connect
	vv, _ = v.Update(keyRunes('j'))
	v = vv.(boxesView)
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter should emit a switch")
	}
	sw, ok := cmd().(switchBoxMsg)
	if !ok || sw.box.Name != "blog" {
		t.Fatalf("want switchBoxMsg for blog, got %#v", cmd())
	}
}

func TestEnterOnLANBoxWithRelayCredsEmitsSwitch(t *testing.T) {
	// A LAN-addressable box that also carries relay creds (a relay-enrolled
	// box on the local network) must still be switchable via its LAN address.
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes:   []config.Box{{Name: "pi4"}, {Name: "cloud", Addr: "192.168.1.6:8088", RelayAPI: "https://r.example"}},
		current: "pi4",
	})
	v = vv.(boxesView)
	vv, _ = v.Update(keyRunes('j'))
	v = vv.(boxesView)
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter on a LAN box with relay creds should emit a switch")
	}
	sw, ok := cmd().(switchBoxMsg)
	if !ok || sw.box.Name != "cloud" {
		t.Fatalf("want switchBoxMsg for cloud, got %#v", cmd())
	}
}

func TestEnterOnRelayOnlyBoxEmitsSwitch(t *testing.T) {
	load := boxesLoadedMsg{
		boxes:   []config.Box{{Name: "pi4"}, {Name: "cloud.example", RelayAPI: "https://r.example", AccountCredential: "cred-xyz"}},
		current: "pi4",
	}
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(load)
	v = vv.(boxesView)
	vv, _ = v.Update(keyRunes('j'))
	v = vv.(boxesView)
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter on a relay-only box should emit a switch")
	}
	msg := cmd()
	sw, ok := msg.(switchBoxMsg)
	if !ok || sw.box.Name != "cloud.example" {
		t.Fatalf("want switchBoxMsg for cloud.example, got %#v", msg)
	}
}

func TestBoxesViewRefreshIncludesRelayAgentsAndUsesRelayLiveness(t *testing.T) {
	relay := agentsServer(t, []relayclient.Agent{{BaseDomain: "cloud.example", Connected: true}})
	defer relay.Close()
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "local", Addr: "192.168.1.6:8088", RelayAPI: relay.URL, AccountCredential: "cred-xyz"}},
		Current: "local",
	})

	var dialed []string
	dial := func(box config.Box) (API, string, bool, error) {
		dialed = append(dialed, box.Name)
		return fakeAPI{}, "", false, nil
	}
	v := newRelayBoxesView(t, dial, func(base string) RelayAPI { return relayclient.New(base) })
	loaded, ok := v.refresh(nil)().(boxesLoadedMsg)
	if !ok {
		t.Fatal("refresh should yield boxesLoadedMsg")
	}
	vv, cmd := v.Update(loaded)
	v = vv.(boxesView)
	if cmd == nil {
		t.Fatal("local rows should schedule the relay fetch")
	}
	vv, _ = v.Update(cmd())
	v = vv.(boxesView)
	if !strings.Contains(v.View(), "cloud.example") || !strings.Contains(v.View(), "●") {
		t.Fatalf("connected relay agent should be listed as live:\n%s", v.View())
	}
	// A LAN probe result must not override the relay's connected field.
	v.reach["cloud.example"] = false
	if !strings.Contains(v.View(), "●") {
		t.Fatalf("relay liveness should come from connected, not a LAN probe:\n%s", v.View())
	}
	for _, name := range dialed {
		if name == "cloud.example" {
			t.Fatal("relay-only row must not be probed through the LAN dialer")
		}
	}

	vv, _ = v.Update(keyRunes('j'))
	v = vv.(boxesView)
	_, cmd = v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("relay-only row should be switchable")
	}
	msg := cmd()
	sw, ok := msg.(switchBoxMsg)
	if !ok || sw.box.Name != "cloud.example" || sw.box.RelayAPI != relay.URL || sw.box.AccountCredential != "cred-xyz" {
		t.Fatalf("relay row switch = %#v", msg)
	}
}

func TestBoxesViewDeduplicatesRowsFromConfigAndRelay(t *testing.T) {
	relay := agentsServer(t, []relayclient.Agent{{BaseDomain: "cloud.example", Connected: true}})
	defer relay.Close()
	seedConfig(t, config.ClientFile{
		Boxes: []config.Box{
			{Name: "account", Addr: "192.168.1.5:8088", RelayAPI: relay.URL, AccountCredential: "cred-xyz"},
			{Name: "cloud.example", RelayAPI: relay.URL, AccountCredential: "cred-xyz"},
		},
		Current: "account",
	})
	v := newRelayBoxesView(t, fakeDialer(fakeAPI{}, "", false, nil), func(base string) RelayAPI { return relayclient.New(base) })
	loaded, ok := v.refresh(nil)().(boxesLoadedMsg)
	if !ok {
		t.Fatal("refresh should yield boxesLoadedMsg")
	}
	vv, cmd := v.Update(loaded)
	v = vv.(boxesView)
	if cmd == nil {
		t.Fatal("local rows should schedule the relay fetch")
	}
	vv, _ = v.Update(cmd())
	v = vv.(boxesView)
	if got := strings.Count(v.View(), "cloud.example"); got != 1 {
		t.Fatalf("box present in config and /agents should render once, got %d rows:\n%s", got, v.View())
	}
	if !strings.Contains(v.View(), "●") {
		t.Fatalf("deduplicated relay row should use relay liveness:\n%s", v.View())
	}
	for _, box := range v.boxes {
		if box.Name == "cloud.example" {
			if box.RelayAPI != relay.URL || box.AccountCredential != "cred-xyz" {
				t.Fatalf("deduplicated row lost relay path: %+v", box)
			}
			return
		}
	}
	t.Fatal("deduplicated relay row missing")
}

func TestBoxesViewDeduplicatedLANRowUsesLANLiveness(t *testing.T) {
	relay := agentsServer(t, []relayclient.Agent{{BaseDomain: "cloud.example", Connected: false}})
	defer relay.Close()
	seedConfig(t, config.ClientFile{
		Boxes: []config.Box{
			{Name: "account", Addr: "192.168.1.5:8088", RelayAPI: relay.URL, AccountCredential: "cred-xyz"},
			{Name: "cloud.example", Addr: "192.168.1.6:8088"},
		},
		Current: "account",
	})

	var dialed []string
	dial := func(box config.Box) (API, string, bool, error) {
		dialed = append(dialed, box.Name)
		return fakeAPI{}, box.Addr, false, nil
	}
	v := newRelayBoxesView(t, dial, func(base string) RelayAPI { return relayclient.New(base) })
	loaded, ok := v.refresh(nil)().(boxesLoadedMsg)
	if !ok {
		t.Fatal("refresh should yield boxesLoadedMsg")
	}
	vv, cmd := v.Update(loaded)
	v = vv.(boxesView)
	if cmd == nil {
		t.Fatal("deduplicated LAN row should emit a LAN probe")
	}
	result := cmd()
	var relayMsg relayAgentsLoadedMsg
	var probe boxProbeMsg
	switch result := result.(type) {
	case tea.BatchMsg:
		for _, subcmd := range result {
			switch msg := subcmd().(type) {
			case relayAgentsLoadedMsg:
				relayMsg = msg
			case boxProbeMsg:
				probe = msg
			}
		}
	case boxProbeMsg:
		probe = result
	case relayAgentsLoadedMsg:
		relayMsg = result
	}
	if relayMsg.relayAPI != "" {
		vv, _ = v.Update(relayMsg)
		v = vv.(boxesView)
	}
	if probe.name != "cloud.example" || !probe.reachable {
		t.Fatalf("want reachable cloud.example LAN probe, got %#v", probe)
	}
	if len(dialed) != 1 || dialed[0] != "cloud.example" {
		t.Fatalf("LAN probe dialed %v, want [cloud.example]", dialed)
	}
	vv, _ = v.Update(probe)
	v = vv.(boxesView)
	if !strings.Contains(v.View(), "cloud.example") || !strings.Contains(v.View(), "●") {
		t.Fatalf("deduplicated LAN row should use LAN liveness when relay is disconnected:\n%s", v.View())
	}
}

type blockingAgentsRelay struct {
	fakeRelay
	started chan<- struct{}
	release <-chan struct{}
}

func (r blockingAgentsRelay) Agents(context.Context, string) ([]relayclient.Agent, error) {
	close(r.started)
	<-r.release
	return r.fakeRelay.agents, r.fakeRelay.agentsErr
}

func TestBoxesRefreshRendersLocalRowsBeforeRelayReturns(t *testing.T) {
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "local", Addr: "192.168.1.6:8088", RelayAPI: "https://relay.example", AccountCredential: "cred-xyz"}},
		Current: "local",
	})
	started := make(chan struct{})
	release := make(chan struct{})
	relay := blockingAgentsRelay{
		fakeRelay: fakeRelay{agents: []relayclient.Agent{{BaseDomain: "cloud.example", Connected: true}}},
		started:   started,
		release:   release,
	}
	v := newRelayBoxesView(t, fakeDialer(fakeAPI{}, "", false, nil), func(string) RelayAPI { return relay })

	result := make(chan tea.Msg, 1)
	go func() { result <- v.refresh(nil)() }()
	select {
	case msg := <-result:
		loaded, ok := msg.(boxesLoadedMsg)
		if !ok {
			t.Fatalf("refresh should yield local boxesLoadedMsg, got %T", msg)
		}
		vv, relayCmd := v.Update(loaded)
		v = vv.(boxesView)
		if relayCmd == nil {
			t.Fatal("loaded local rows should schedule the relay fetch")
		}
		relayResult := make(chan tea.Msg, 1)
		go func() { relayResult <- relayCmd() }()
		<-started
		if !strings.Contains(v.View(), "local") || strings.Contains(v.View(), "loading") {
			t.Fatalf("local rows should render before relay returns:\n%s", v.View())
		}
		close(release)
		vv, _ = v.Update(<-relayResult)
		v = vv.(boxesView)
		if !strings.Contains(v.View(), "cloud.example") {
			t.Fatalf("relay row should arrive after the controlled release:\n%s", v.View())
		}
	case <-started:
		close(release)
		msg := <-result
		t.Fatalf("refresh waited for relay before returning local rows; got %T after release", msg)
	}
}

func TestBoxesRefreshProbesLANBoxWithRelayCreds(t *testing.T) {
	// Without a live /agents entry, a LAN box carrying stale relay creds still
	// gets a local reachability probe like any other LAN box.
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	_, cmd := v.Update(boxesLoadedMsg{
		boxes: []config.Box{
			{Name: "pi4"},
			{Name: "cloud", Addr: "192.168.1.6:8088", RelayAPI: "https://r.example"},
			{Name: "faraway", RelayAPI: "https://r.example"},
		},
		current: "pi4",
	})
	if cmd == nil {
		t.Fatal("loading boxes should emit reachability probes")
	}
	// tea.Batch collapses a single cmd, so exactly one probe (cloud) yields a
	// bare boxProbeMsg; faraway (relay-only) and pi4 (current) are skipped.
	probe, ok := cmd().(boxProbeMsg)
	if !ok || probe.name != "cloud" {
		t.Fatalf("want a single probe for cloud, got %#v", cmd())
	}
}

func TestRootSwitchSwapsBoxAndResetsStack(t *testing.T) {
	m := NewModel("pi4", "192.168.1.6:8088", false, fakeAPI{}).
		WithDialer(fakeDialer(fakeAPI{apps: nil}, "192.168.1.9:8088", false, nil))
	// go deep so we can prove the stack resets
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	m2, _ = m.Update(switchBoxMsg{box: config.Box{Name: "blog", Addr: "192.168.1.9:8088"}})
	m = m2.(Model)
	if len(m.stack) != 1 {
		t.Fatalf("switch should reset to a single apps view, got depth %d", len(m.stack))
	}
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "blog") || !strings.Contains(out, "192.168.1.9:8088") {
		t.Fatalf("status bar did not switch to blog:\n%s", out)
	}
}

func TestRootSwitchFailureBannersAndKeepsBox(t *testing.T) {
	m := NewModel("pi4", "192.168.1.6:8088", false, fakeAPI{}).
		WithDialer(fakeDialer(nil, "", false, errors.New("dial refused")))
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	m2, _ = m.Update(switchBoxMsg{box: config.Box{Name: "blog", Addr: "x"}})
	m = m2.(Model)
	if m.box != "pi4" {
		t.Fatalf("failed switch must keep the old box, got %q", m.box)
	}
	if !strings.Contains(m.View(), "dial refused") {
		t.Fatalf("switch error should banner in the boxes view:\n%s", m.View())
	}
}

func TestBoxesRefreshEmitsProbePerRemoteBox(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	// current box (pi4) is not probed; blog and shop are.
	vv, cmd := v.Update(boxesLoadedMsg{
		boxes:   []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}, {Name: "shop", Addr: "b"}},
		current: "pi4",
	})
	_ = vv
	if cmd == nil {
		t.Fatal("loading boxes should emit reachability probes")
	}
	msg := cmd() // tea.Batch aggregates into a BatchMsg of cmds
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("want tea.BatchMsg of probes, got %T", msg)
	}
	if len(batch) != 2 {
		t.Fatalf("want 2 probes (non-current, non-relay), got %d", len(batch))
	}
}

func TestBoxProbeMsgFlipsRowStatus(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{boxes: []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}}, current: "pi4"})
	v = vv.(boxesView)

	vv, _ = v.Update(boxProbeMsg{name: "blog", reachable: true})
	if out := vv.(boxesView).View(); !strings.Contains(out, "●") {
		t.Fatalf("reachable box should show ●:\n%s", out)
	}

	vv, _ = v.Update(boxProbeMsg{name: "blog", reachable: false})
	if out := vv.(boxesView).View(); !strings.Contains(out, "○") {
		t.Fatalf("unreachable box should show ○:\n%s", out)
	}
}

func TestBoxProbeReflectsDialerResult(t *testing.T) {
	// a dialer whose client ListApps errors => unreachable
	v := newBoxesView(fakeDialer(fakeAPI{err: errors.New("refused")}, "", false, nil))
	probe := v.probe(config.Box{Name: "blog", Addr: "a"})
	msg := probe().(boxProbeMsg)
	if msg.name != "blog" || msg.reachable {
		t.Fatalf("want blog unreachable, got %#v", msg)
	}
}

func TestXOpensRemoveConfirm(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{boxes: []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}}, current: "pi4"})
	v = vv.(boxesView)
	vv, _ = v.Update(keyRunes('j')) // move to blog
	v = vv.(boxesView)
	_, cmd := v.Update(keyRunes('x'))
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("x should push a confirm, got %T", cmd())
	}
	if _, ok := push.view.(confirmView); !ok {
		t.Fatalf("x should push confirmView, got %T", push.view)
	}
}

func TestRemoveBoxDropsIt(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4"}, {Name: "blog"}}, Current: "pi4"})
	current, changed, err := removeBox("blog")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatalf("removing a non-current box should not change current: %+v", current)
	}
	cf, _ := config.LoadClientFile()
	if len(cf.Boxes) != 1 || cf.Boxes[0].Name != "pi4" {
		t.Fatalf("blog not removed: %+v", cf.Boxes)
	}
}

func TestRemoveCurrentBoxPromotesFirst(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4"}, {Name: "blog"}}, Current: "pi4"})
	current, changed, err := removeBox("pi4")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || current.Name != "blog" {
		t.Fatalf("removing current should promote blog, got changed=%v current=%+v", changed, current)
	}
	cf, _ := config.LoadClientFile()
	if cf.Current != "blog" {
		t.Fatalf("current not promoted on disk: %q", cf.Current)
	}
}

func TestRemoveLastBoxRefused(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4"}}, Current: "pi4"})
	if _, _, err := removeBox("pi4"); err == nil {
		t.Fatal("removing the last box must be refused")
	}
	cf, _ := config.LoadClientFile()
	if len(cf.Boxes) != 1 {
		t.Fatalf("refused remove must not write: %+v", cf.Boxes)
	}
}

func TestRootBoxRemovedNonCurrentPopsToBoxes(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	m2, _ = m.Update(pushMsg{newRemoveBoxConfirm("blog")})
	m = m2.(Model)
	if len(m.stack) != 3 {
		t.Fatalf("setup: want depth 3, got %d", len(m.stack))
	}
	m2, _ = m.Update(boxRemovedMsg{changed: false})
	m = m2.(Model)
	if len(m.stack) != 2 {
		t.Fatalf("removed (non-current) should pop to boxes (depth 2), got %d", len(m.stack))
	}
}
