package tui

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/enrollapi"
	"github.com/piperbox/piper/internal/relayclient"
)

// boxesView is the depth-1 box switcher/editor: a table of the configured boxes
// read fresh from the client config and the account's live relay enrollment
// list. ↵ connects (switches the active box), a/e add/edit via a form, x
// removes. It is the one view that owns local config state rather than piperd
// state.
type boxesView struct {
	dial    Dialer
	relay   RelayDialer
	boxes   []config.Box
	current string
	loaded  bool
	cursor  int
	err     error
	reach   map[string]bool // box name -> last LAN probe result; absent = probing
	// relayConnected is populated from the account's /agents listing for relay
	// rows; those rows must never be probed through a LAN address. Keys are the
	// persisted relay base domains, not display names.
	relayConnected map[string]bool
	// relayRows marks rows synthesized from the live relay listing. They have
	// no local config entry and are therefore switch-only.
	relayRows           map[string]bool
	configBoxes         []config.Box
	relayAgents         []relayclient.Agent
	relayAPI            string
	credential          string
	relayCredentialHash string
	relayLoaded         bool
	viewID              uint64
	// configGeneration changes when the loaded config or selected relay
	// credentials change. Relay results are accepted only for the generation
	// that launched them.
	configGeneration uint64
	latestRefreshID  uint64
	// relayRequestGeneration distinguishes successive relay fetches within one
	// view, including a replacement launched after a config change.
	relayRequestGeneration uint64
	// relayFetchStarted is intentionally one-shot for a view instance: relay
	// liveness is fetched on entry, while the regular tick only refreshes local
	// config and LAN probes.
	relayFetchStarted bool
}

var (
	boxesViewSequence    uint64
	boxesRefreshSequence uint64
)

func newBoxesView(dial Dialer) boxesView {
	return boxesView{
		dial:           dial,
		viewID:         atomic.AddUint64(&boxesViewSequence, 1),
		reach:          map[string]bool{},
		relayConnected: map[string]bool{},
		relayRows:      map[string]bool{},
	}
}

func (v boxesView) Init() tea.Cmd { return nil }

func (v boxesView) title() string { return "boxes" }

func (v boxesView) footer() string {
	return "↵ connect · a add · e edit · x remove · esc back"
}

// refresh reloads the client config off the UI thread. The optional relay
// enrollment fetch is scheduled after this local result is rendered, so a slow
// relay cannot hold the saved LAN rows in loading state.
func (v boxesView) refresh(API) tea.Cmd {
	viewID := v.viewID
	requestID := atomic.AddUint64(&boxesRefreshSequence, 1)
	return func() tea.Msg {
		cf, err := config.LoadClientFile()
		if err != nil {
			return errMsg{err}
		}
		relayAPI, credential := relayCredentials(cf)
		return boxesLoadedMsg{
			boxes:               cf.Boxes,
			current:             cf.Current,
			relayAPI:            relayAPI,
			credential:          credential,
			relayCredentialHash: relayCredentialHash(cf),
			viewID:              viewID,
			requestID:           requestID,
		}
	}
}

// fetchRelay loads the saved account's relay enrollment list off the UI
// thread. Local rows are already visible by the time this command runs; a
// relay failure is shown as a view error without removing those rows.
func (v boxesView) fetchRelay(configGeneration, requestGeneration uint64) tea.Cmd {
	relay := v.relay
	relayAPI, credential := v.relayAPI, v.credential
	relayCredentialHash := v.relayCredentialHash
	configBoxes := append([]config.Box(nil), v.configBoxes...)
	dial := v.dial
	viewID := v.viewID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), relayRequestTimeout)
		defer cancel()
		agents, err := relay(relayAPI).Agents(ctx, credential)
		identities := localRelayIdentities(dial, configBoxes, agents)
		return relayAgentsLoadedMsg{
			agents:              agents,
			identities:          identities,
			err:                 err,
			viewID:              viewID,
			configGeneration:    configGeneration,
			requestGeneration:   requestGeneration,
			relayAPI:            relayAPI,
			credential:          credential,
			relayCredentialHash: relayCredentialHash,
		}
	}
}

const relayRequestTimeout = 5 * time.Second

func relayCredentials(cf config.ClientFile) (string, string) {
	if current, ok := cf.CurrentBox(); ok && current.RelayAPI != "" && current.AccountCredential != "" {
		return current.RelayAPI, current.AccountCredential
	}
	for _, box := range cf.Boxes {
		if box.RelayAPI != "" && box.AccountCredential != "" {
			return box.RelayAPI, box.AccountCredential
		}
	}
	return "", ""
}

func relayCredentialHash(cf config.ClientFile) string {
	var b strings.Builder
	for _, box := range cf.Boxes {
		fmt.Fprintf(&b, "%d:%s%d:%s%d:%s\x00", len(box.RelayAPI), box.RelayAPI, len(box.AccountCredential), box.AccountCredential, len(box.Name), box.Name)
	}
	return b.String()
}

func boxIdentityKey(box config.Box) string {
	var b strings.Builder
	for _, value := range []string{box.Name, box.Addr, box.Token, box.BaseDomain, box.RelayAPI, box.AccountCredential} {
		fmt.Fprintf(&b, "%d:%s\x00", len(value), value)
	}
	return b.String()
}

type relayStatusReader interface {
	RelayStatus() (enrollapi.Status, error)
}

func localRelayIdentities(dial Dialer, boxes []config.Box, agents []relayclient.Agent) map[string]string {
	identities := make(map[string]string)
	if dial == nil {
		return identities
	}
	// Names only narrow which local daemons need a status read. They are never
	// used as identity evidence: migration still requires the daemon's own
	// BaseDomain and a matching BaseDomain in the fetched relay listing.
	candidates := make(map[string]struct{}, len(agents)*2)
	for _, agent := range agents {
		if agent.Name != "" {
			candidates[agent.Name] = struct{}{}
		}
		if agent.BaseDomain != "" {
			candidates[agent.BaseDomain] = struct{}{}
		}
	}
	for _, box := range boxes {
		if box.BaseDomain != "" || box.Addr == "" {
			continue
		}
		if _, ok := candidates[box.Name]; !ok {
			continue
		}
		api, _, _, err := dial(box)
		if err != nil || api == nil {
			continue
		}
		reader, ok := api.(relayStatusReader)
		if !ok {
			continue
		}
		status, err := reader.RelayStatus()
		if err == nil && status.BaseDomain != "" {
			identities[boxIdentityKey(box)] = status.BaseDomain
		}
	}
	return identities
}

// mergeBoxes keeps saved LAN entries (including their names and addresses) and
// appends live relay agents that are not already present. A matching local row
// is identified by its persisted BaseDomain, receives the relay credentials,
// and keeps its LAN path as the preferred dial path. Relay liveness is used
// only by relay-only rows.
func mergeBoxes(local []config.Box, agents []relayclient.Agent, relayAPI, credential string) ([]config.Box, map[string]bool, map[string]bool) {
	boxes := make([]config.Box, 0, len(local)+len(agents))
	byName := make(map[string]int, len(local)+len(agents))
	byBaseDomain := make(map[string]int, len(local))
	for _, box := range local {
		if _, exists := byName[box.Name]; exists {
			continue
		}
		idx := len(boxes)
		byName[box.Name] = idx
		if box.BaseDomain != "" {
			byBaseDomain[box.BaseDomain] = idx
		}
		boxes = append(boxes, box)
	}

	connected := make(map[string]bool, len(agents))
	relayRows := make(map[string]bool, len(agents))
	for _, agent := range agents {
		if agent.BaseDomain == "" {
			continue
		}
		idx, exists := byBaseDomain[agent.BaseDomain]
		if exists {
			boxes[idx].RelayAPI = relayAPI
			boxes[idx].AccountCredential = credential
		} else {
			name := agent.Name
			if name == "" {
				name = agent.BaseDomain
			}
			byName[name] = len(boxes)
			boxes = append(boxes, config.Box{
				Name:              name,
				BaseDomain:        agent.BaseDomain,
				RelayAPI:          relayAPI,
				AccountCredential: credential,
			})
			relayRows[agent.BaseDomain] = true
		}
		connected[agent.BaseDomain] = agent.Connected
	}
	return boxes, connected, relayRows
}

// migrateLegacyBoxes gives pre-base_domain configs a safe one-time upgrade
// path. The identity evidence is read from the enrolled local piperd and must
// also occur in the current relay listing; display names are never evidence.
func migrateLegacyBoxes(local []config.Box, agents []relayclient.Agent, identities map[string]string) ([]config.Box, bool) {
	boxes := append([]config.Box(nil), local...)
	knownBases := make(map[string]struct{}, len(agents))
	for _, agent := range agents {
		if agent.BaseDomain != "" {
			knownBases[agent.BaseDomain] = struct{}{}
		}
	}
	changed := false
	for i, box := range boxes {
		if box.BaseDomain != "" {
			continue
		}
		baseDomain, ok := identities[boxIdentityKey(box)]
		if !ok {
			continue
		}
		if _, ok := knownBases[baseDomain]; !ok {
			continue
		}
		boxes[i].BaseDomain = baseDomain
		changed = true
	}
	return boxes, changed
}

func persistLegacyIdentities(expectedHash, relayAPI, credential string, agents []relayclient.Agent, identities map[string]string) (config.ClientFile, bool, error) {
	var current config.ClientFile
	accepted := false
	err := config.UpdateClientFile(func(cf *config.ClientFile) (bool, error) {
		current = *cf
		currentAPI, currentCredential := relayCredentials(*cf)
		if currentAPI != relayAPI || currentCredential != credential {
			return false, nil
		}
		if expectedHash != "" && relayCredentialHash(*cf) != expectedHash {
			return false, nil
		}
		migrated, changed := migrateLegacyBoxes(cf.Boxes, agents, identities)
		if !changed {
			accepted = true
			return false, nil
		}
		cf.Boxes = migrated
		current = *cf
		accepted = true
		return true, nil
	})
	return current, accepted, err
}

// relayOnly reports whether the box at i has no LAN path. LAN-addressable rows
// keep their LAN switch/probe path even when relay credentials are present;
// relay-only rows use the live relay status when listed.
func (v boxesView) relayOnly(i int) bool {
	return v.boxes[i].RelayAPI != "" && v.boxes[i].Addr == ""
}

func relayKey(box config.Box) string {
	if box.BaseDomain != "" {
		return box.BaseDomain
	}
	return box.Name
}

func (v boxesView) relayRow(box config.Box) bool {
	_, listed := v.relayConnected[relayKey(box)]
	return listed
}

func (v boxesView) liveRelayRow(box config.Box) bool {
	return v.relayRows[relayKey(box)]
}

// rowIdentity is the selection key for a rendered row. BaseDomain is the
// stable relay identity for both live-only and config-backed rows; provenance
// controls permissions separately through liveRelayRow.
func (v boxesView) rowIdentity(box config.Box) string {
	if box.BaseDomain != "" {
		return "relay:" + box.BaseDomain
	}
	return "config:" + box.Name + "\x00" + box.Addr + "\x00" + box.Token
}

// probe returns a cmd that dials box and calls ListApps; reachable is true iff
// both succeed. One cmd per box keeps a dead box from blocking the others.
func (v boxesView) probe(box config.Box) tea.Cmd {
	dial := v.dial
	return func() tea.Msg {
		c, _, _, err := dial(box)
		if err == nil {
			_, err = c.ListApps()
		}
		return boxProbeMsg{name: box.Name, reachable: err == nil}
	}
}

func (v boxesView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case boxesLoadedMsg:
		if msg.viewID != 0 && msg.viewID != v.viewID {
			return v, nil
		}
		if msg.requestID != 0 && msg.requestID < v.latestRefreshID {
			return v, nil
		}
		selected := ""
		if v.cursor >= 0 && v.cursor < len(v.boxes) {
			selected = v.rowIdentity(v.boxes[v.cursor])
		}
		configChanged := !reflect.DeepEqual(v.configBoxes, msg.boxes) || v.current != msg.current
		credentialsChanged := v.relayAPI != msg.relayAPI || v.credential != msg.credential ||
			(v.relayCredentialHash != "" && msg.relayCredentialHash != "" && v.relayCredentialHash != msg.relayCredentialHash)
		if v.configGeneration == 0 {
			v.configGeneration = 1
		} else if configChanged || credentialsChanged {
			v.configGeneration++
		}
		if msg.requestID != 0 {
			v.latestRefreshID = msg.requestID
		}
		v.configBoxes, v.current, v.loaded = append([]config.Box(nil), msg.boxes...), msg.current, true
		v.relayAPI, v.credential = msg.relayAPI, msg.credential
		v.relayCredentialHash = msg.relayCredentialHash
		if credentialsChanged {
			v.relayAgents = nil
			v.relayLoaded = false
			v.relayConnected = map[string]bool{}
			v.relayRows = map[string]bool{}
			v.relayFetchStarted = false
		} else if configChanged && !v.relayLoaded {
			// An in-flight result for the old config must be rejected; allow a
			// replacement request for the new config to establish a fresh snapshot.
			v.relayFetchStarted = false
		}
		if v.relayLoaded {
			v.boxes, v.relayConnected, v.relayRows = mergeBoxes(v.configBoxes, v.relayAgents, v.relayAPI, v.credential)
		} else {
			v.boxes = append([]config.Box(nil), v.configBoxes...)
		}
		v.reach = keepReachable(v.reach, v.boxes)
		if v.cursor >= len(v.boxes) {
			v.cursor = max(0, len(v.boxes)-1)
		}
		if selected != "" {
			for i, box := range v.boxes {
				if v.rowIdentity(box) == selected {
					v.cursor = i
					break
				}
			}
		}
		var cmds []tea.Cmd
		if !v.relayFetchStarted && v.relay != nil && msg.relayAPI != "" && msg.credential != "" {
			v.relayFetchStarted = true
			v.relayRequestGeneration++
			cmds = append(cmds, v.fetchRelay(v.configGeneration, v.relayRequestGeneration))
		}
		for i, box := range v.boxes {
			if box.Name == v.current || v.relayOnly(i) {
				continue
			}
			cmds = append(cmds, v.probe(box))
		}
		return v, tea.Batch(cmds...)
	case relayAgentsLoadedMsg:
		if msg.viewID != 0 && msg.viewID != v.viewID {
			return v, nil
		}
		if msg.configGeneration != 0 && msg.configGeneration != v.configGeneration {
			return v, nil
		}
		if msg.requestGeneration != 0 && msg.requestGeneration != v.relayRequestGeneration {
			return v, nil
		}
		if (msg.relayAPI != "" || msg.credential != "") &&
			(msg.relayAPI != v.relayAPI || msg.credential != v.credential) {
			return v, nil
		}
		if msg.relayCredentialHash != "" && v.relayCredentialHash != "" && msg.relayCredentialHash != v.relayCredentialHash {
			return v, nil
		}
		if msg.err != nil {
			v.err = msg.err
			return v, nil
		}
		selected := ""
		if v.cursor >= 0 && v.cursor < len(v.boxes) {
			selected = v.rowIdentity(v.boxes[v.cursor])
		}
		if msg.requestGeneration != 0 {
			cf, accepted, err := persistLegacyIdentities(msg.relayCredentialHash, msg.relayAPI, msg.credential, msg.agents, msg.identities)
			if err != nil {
				v.err = err
				return v, nil
			}
			if !accepted {
				return v, nil
			}
			v.configBoxes = append([]config.Box(nil), cf.Boxes...)
			v.current = cf.Current
		} else {
			migrated, changed := migrateLegacyBoxes(v.configBoxes, msg.agents, msg.identities)
			if changed {
				v.configBoxes = migrated
			}
		}
		v.relayAgents = append([]relayclient.Agent(nil), msg.agents...)
		v.relayLoaded = true
		v.boxes, v.relayConnected, v.relayRows = mergeBoxes(v.configBoxes, v.relayAgents, v.relayAPI, v.credential)
		v.err = nil
		if v.cursor >= len(v.boxes) {
			v.cursor = max(0, len(v.boxes)-1)
		}
		if selected != "" {
			for i, box := range v.boxes {
				if v.rowIdentity(box) == selected {
					v.cursor = i
					break
				}
			}
		}
		return v, nil
	case boxProbeMsg:
		v.reach[msg.name] = msg.reachable
	case errMsg:
		v.err = msg.err
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if v.cursor > 0 {
				v.cursor--
			}
		case "down", "j":
			if v.cursor < len(v.boxes)-1 {
				v.cursor++
			}
		case "enter":
			if len(v.boxes) == 0 {
				break
			}
			box := v.boxes[v.cursor]
			return v, func() tea.Msg { return switchBoxMsg{box: box} }
		case "a":
			boxes := v.boxes
			return v, func() tea.Msg { return pushMsg{newBoxForm(v.dial, boxes)} }
		case "e":
			if len(v.boxes) > 0 && !v.liveRelayRow(v.boxes[v.cursor]) {
				boxes, orig := v.boxes, v.boxes[v.cursor]
				return v, func() tea.Msg { return pushMsg{newBoxFormEdit(v.dial, boxes, orig)} }
			}
		case "x":
			if len(v.boxes) > 0 && !v.liveRelayRow(v.boxes[v.cursor]) {
				name := v.boxes[v.cursor].Name
				return v, func() tea.Msg { return pushMsg{newRemoveBoxConfirm(name)} }
			}
		}
	}
	return v, nil
}

func keepReachable(previous map[string]bool, boxes []config.Box) map[string]bool {
	kept := make(map[string]bool, len(previous))
	for _, box := range boxes {
		if reachable, ok := previous[box.Name]; ok {
			kept[box.Name] = reachable
		}
	}
	return kept
}

func (v boxesView) View() string {
	var b strings.Builder
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-16s %-22s %s\n", "NAME", "ADDR", "STATUS")
	for i, box := range v.boxes {
		cursor := "  "
		if i == v.cursor {
			cursor = "▸ "
		}
		fmt.Fprintf(&b, "%s%-16s %-22s %s\n", cursor, box.Name, box.Addr, v.status(i))
	}
	return b.String()
}

func (v boxesView) status(i int) string {
	switch {
	case v.boxes[i].Name == v.current:
		return "current"
	case v.relayOnly(i):
		if v.relayRow(v.boxes[i]) {
			if v.relayConnected[relayKey(v.boxes[i])] {
				return "●"
			}
			return "○"
		}
		return "—"
	}
	reachable, probed := v.reach[v.boxes[i].Name]
	switch {
	case !probed:
		return "…"
	case reachable:
		return "●"
	default:
		return "○"
	}
}

// saveBox writes box to the client config: it updates the box named replacing
// (whose name may change), else appends box. All other boxes are preserved; a
// first box (empty config) becomes current. replacing == "" means add.
func saveBox(box config.Box, replacing string) error {
	return config.UpdateClientFile(func(cf *config.ClientFile) (bool, error) {
		if replacing != "" {
			for i := range cf.Boxes {
				if cf.Boxes[i].Name == replacing {
					if cf.Current == replacing {
						cf.Current = box.Name
					}
					cf.Boxes[i] = box
					return true, nil
				}
			}
		}
		cf.Boxes = append(cf.Boxes, box)
		if cf.Current == "" {
			cf.Current = box.Name
		}
		return true, nil
	})
}

// removeBox drops the box named name from the client config. If it was the
// current box, the first remaining box is promoted and returned with
// changed=true. Removing the last box is refused (the CLI always needs one).
func removeBox(name string) (current config.Box, changed bool, err error) {
	err = config.UpdateClientFile(func(cf *config.ClientFile) (bool, error) {
		if len(cf.Boxes) <= 1 {
			return false, fmt.Errorf("can't remove the last box")
		}
		kept := cf.Boxes[:0]
		for _, b := range cf.Boxes {
			if b.Name != name {
				kept = append(kept, b)
			}
		}
		cf.Boxes = kept
		if cf.Current == name {
			cf.Current = cf.Boxes[0].Name
			current, changed = cf.Boxes[0], true
		}
		return true, nil
	})
	return current, changed, err
}
