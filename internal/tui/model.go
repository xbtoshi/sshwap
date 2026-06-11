package tui

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	zone "github.com/lrstanley/bubblezone"

	"github.com/kyc-rip/cli/internal/api"
)

// zone IDs — string constants used to mark interactive regions and
// hit-test mouse clicks against them. Each session has its own
// zone manager via NewModel() so concurrent SSH sessions don't collide.
const (
	zTabSwap   = "tab-swap"
	zTabGhost  = "tab-ghost"
	zTabTrack  = "tab-track"
	zTabAbout  = "tab-about"
	zButton    = "button-primary"
	zAssetRow  = "asset-row-" // suffixed with index 0..8
	zAddressOK = "addr-ok"
)

// --- tabs ---

type tab int

const (
	tabSwap tab = iota
	tabGhost
	tabTrack
	tabAbout // not in main tab bar; reachable via 'a' key
)

// --- swap wizard states ---

type swapState int

const (
	stPickFrom swapState = iota
	stPickTo
	stAmount
	stAddress
	stMemo // shown only when destination needs memo
	stQuoting
	stQuoted
	stCreating
	stOrdered
	stError
)

const (
	pollInterval = 5 * time.Second
	apiTimeout   = 12 * time.Second
)

// --- config ---

type Config struct {
	Client      *api.Client
	Fingerprint string
	Username    string // SSH session username for the @swap header tag

	// DryRun stops the wizard at the Quote step. Pressing Confirm shows
	// the would-be order shape but never calls POST /create. Useful for
	// integration tests and 'try before you commit funds' tours.
	DryRun bool

	// InitialWidth / InitialHeight let SSH hosts seed dimensions before
	// the first WindowSizeMsg arrives, so the alt-screen flip on
	// connection-start renders the form immediately instead of a void.
	InitialWidth  int
	InitialHeight int

	// ClipboardWriter is the same io.Writer Bubble Tea is configured with
	// via tea.WithOutput, wrapped in a mutex (see LockedWriter). The model
	// writes OSC 52 clipboard escapes directly through it instead of
	// embedding them in View() output — bubbletea's renderer is not a
	// safe transport for device-control sequences.
	ClipboardWriter *LockedWriter

	// LocalActions enables OS-level clipboard/browser operations. This is
	// only valid for the local kyc-cli binary; the SSH host cannot touch
	// the user's workstation clipboard or browser except through terminal
	// escape sequences.
	LocalActions bool
}

// --- model ---

type Model struct {
	cfg Config

	zm *zone.Manager // per-session zone manager for mouse hit-testing

	width, height int
	tab           tab

	// wizard state
	state        swapState
	from         string // "BTC" or "USDT-TRC20"
	to           string
	amtIn        textinput.Model
	addrIn       textinput.Model
	memoIn       textinput.Model
	quote        *api.Estimate
	picks        routePicks
	routePick    routeMode // currently-selected bucket; "" before quote arrives
	trade        *api.Trade
	swapErr      string
	pollOn       bool
	qrFullScreen bool   // 'q' in stOrdered or Track expands the QR to fill the terminal.
	qrImageMode  bool   // 'g' toggles iTerm2 inline-image protocol — Warp/iTerm/WezTerm.
	copyToast    string // ephemeral "📋 copied …" feedback; cleared by clearToastMsg.
	depositFocus int    // 0 = address, 1 = QR URL. up/down cycles, enter copies.
	ghostMode    bool   // true while on tabGhost — routes API calls through /v2/exchange/bridge.
	selectMode   bool   // 'm' toggles: when true, mouse reporting is off so the user's
	// terminal handles native click-drag-to-select. Tabs/buttons stop
	// firing on click while it's on; 'm' again restores click nav.

	// track tab
	trackIn    textinput.Model
	trackTrade *api.Trade
	trackErr   string
	trackBusy  bool
	trackPoll  bool // auto-refresh until terminal status reached
}

func New(cfg Config) Model {
	mk := func(ph string, w int) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = ph
		ti.CharLimit = 128
		ti.Width = w
		ti.Prompt = ""
		return ti
	}
	m := Model{
		cfg:     cfg,
		zm:      zone.New(),
		tab:     tabSwap,
		state:   stPickFrom,
		amtIn:   mk("e.g. 0.01", 24),
		addrIn:  mk("destination wallet address", 60),
		memoIn:  mk("optional memo / dest tag", 30),
		trackIn: mk("paste trade id", 40),
	}
	if cfg.InitialWidth > 0 && cfg.InitialHeight > 0 {
		m.width = cfg.InitialWidth
		m.height = cfg.InitialHeight
	}
	return m
}

func (m Model) Init() tea.Cmd { return textinput.Blink }

// --- messages ---

type estimateDoneMsg struct {
	q   *api.Estimate
	err error
}
type tradeDoneMsg struct {
	t   *api.Trade
	err error
}
type statusDoneMsg struct {
	t       *api.Trade
	err     error
	isTrack bool
}
type tickMsg time.Time
type clearToastMsg struct{}

// --- commands ---

func (m Model) cmdEstimate() tea.Cmd {
	cli := m.cfg.Client
	from, fromNet := splitTickerNet(m.from)
	to, toNet := splitTickerNet(m.to)
	amt, _ := strconv.ParseFloat(strings.TrimSpace(m.amtIn.Value()), 64)
	ghost := m.ghostMode
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		var q *api.Estimate
		var err error
		if ghost {
			q, err = cli.EstimateBridge(ctx, from, fromNet, to, toNet, amt)
		} else {
			q, err = cli.Estimate(ctx, from, fromNet, to, toNet, amt)
		}
		return estimateDoneMsg{q, err}
	}
}

func (m Model) cmdCreate() tea.Cmd {
	cli := m.cfg.Client
	from, fromNet := splitTickerNet(m.from)
	to, toNet := splitTickerNet(m.to)
	amt, _ := strconv.ParseFloat(strings.TrimSpace(m.amtIn.Value()), 64)
	addr := strings.TrimSpace(m.addrIn.Value())
	memo := strings.TrimSpace(m.memoIn.Value())

	provider := m.quote.Provider
	engine := m.quote.Engine
	amountTo := m.quote.AmountTo
	fixed := false
	var hq any
	if r := m.picks.get(m.routePick); r != nil {
		provider = r.Provider
		engine = r.Engine
		amountTo = r.AmountTo
		fixed = r.Fixed
		hq = r.HoudiniQuote
	} else if len(m.quote.Routes) > 0 {
		provider = m.quote.Routes[0].Provider
		engine = m.quote.Routes[0].Engine
		amountTo = m.quote.Routes[0].AmountTo
		fixed = m.quote.Routes[0].Fixed
		hq = m.quote.Routes[0].HoudiniQuote
	}
	req := api.CreateReq{
		TradeID:      m.quote.ID,
		Provider:     provider,
		Engine:       engine,
		FromCurrency: from,
		ToCurrency:   to,
		FromNetwork:  fromNet,
		ToNetwork:    toNet,
		AmountFrom:   amt,
		AmountTo:     amountTo,
		AddressTo:    addr,
		FixedRate:    fixed,
		AddressMemo:  memo,
		HoudiniQuote: hq,
	}
	// Ghost (bridge) routes that need a refund address default to the
	// destination address — same fallback the Telegram bot uses. Better
	// than failing the order; user can override later via a future field.
	if m.ghostMode {
		req.RefundAddress = addr
	}
	ghost := m.ghostMode
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		var t *api.Trade
		var err error
		if ghost {
			t, err = cli.CreateBridge(ctx, req)
		} else {
			t, err = cli.Create(ctx, req)
		}
		return tradeDoneMsg{t, err}
	}
}

func (m Model) cmdStatus(id string, isTrack bool) tea.Cmd {
	cli := m.cfg.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		t, err := cli.Status(ctx, id)
		return statusDoneMsg{t, err, isTrack}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// --- update ---

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.MouseMsg:
		if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
			return m, nil
		}
		// Tab clicks (any state)
		if m.zm.Get(zTabSwap).InBounds(msg) {
			m.tab = tabSwap
			m.ghostMode = false
			m.resetSwap()
			return m, nil
		}
		if m.zm.Get(zTabGhost).InBounds(msg) {
			m.tab = tabGhost
			m.ghostMode = true
			m.resetSwap()
			return m, nil
		}
		if m.zm.Get(zTabTrack).InBounds(msg) {
			m.tab = tabTrack
			m.trackIn.Focus()
			return m, nil
		}
		// Button click → synthesize Enter
		if m.zm.Get(zButton).InBounds(msg) {
			return m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		}
		// Asset row click in pickers
		if m.tab == tabSwap && (m.state == stPickFrom || m.state == stPickTo) {
			for i := 0; i < 9 && i < len(topAssets); i++ {
				if m.zm.Get(zAssetRow + strconv.Itoa(i)).InBounds(msg) {
					m.assignAsset(topAssets[i])
					return m, nil
				}
			}
		}
		return m, nil

	case tea.KeyMsg:
		// Always-on shortcuts — fire even while a textinput is focused,
		// otherwise the user can't escape into select-mode while typing
		// an address.
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			return m, tea.Quit
		case "ctrl+s":
			m.selectMode = !m.selectMode
			if m.selectMode {
				m.copyToast = "🖱 select mode — drag to select · ctrl+s for clicks"
				return m, tea.Batch(
					tea.DisableMouse,
					tea.Tick(3*time.Second, func(_ time.Time) tea.Msg { return clearToastMsg{} }),
				)
			}
			m.copyToast = "🖱 click mode — ctrl+s for select"
			return m, tea.Batch(
				tea.EnableMouseCellMotion,
				tea.Tick(2*time.Second, func(_ time.Time) tea.Msg { return clearToastMsg{} }),
			)
		}

		// Global hotkeys (only when NOT actively typing into the active step)
		if !m.isTypingState() {
			switch msg.String() {
			case "s":
				m.tab = tabSwap
				m.ghostMode = false
				m.resetSwap()
				return m, nil
			case "g":
				m.tab = tabGhost
				m.ghostMode = true
				m.resetSwap()
				return m, nil
			case "t":
				m.tab = tabTrack
				m.trackIn.Focus()
				return m, nil
			case "a":
				m.tab = tabAbout
				return m, nil
			}
		}

		// Deposit-panel keyboard nav fires before tab dispatch so arrows
		// don't get eaten by updateTrack's textinput forwarding.
		if newM, cmd, handled := m.handleDepositKeys(msg); handled {
			return newM, cmd
		}

		if m.tab == tabSwap || m.tab == tabGhost {
			return m.updateSwap(msg)
		}
		if m.tab == tabTrack {
			return m.updateTrack(msg)
		}
		// About tab — any key returns to swap
		if msg.String() == "esc" || msg.String() == "enter" {
			m.tab = tabSwap
		}
		return m, nil

	case estimateDoneMsg:
		if msg.err != nil {
			m.state = stError
			m.swapErr = msg.err.Error()
			return m, nil
		}
		m.quote = msg.q
		m.picks = pickRoutesByMode(msg.q.Routes)
		m.routePick = ""
		for _, rm := range m.picks.modes() {
			m.routePick = rm
			break
		}
		m.state = stQuoted
		return m, nil

	case tradeDoneMsg:
		if msg.err != nil {
			m.state = stError
			m.swapErr = msg.err.Error()
			return m, nil
		}
		m.trade = msg.t
		m.state = stOrdered
		m.pollOn = true
		return m, tickCmd()

	case statusDoneMsg:
		if msg.isTrack {
			m.trackBusy = false
			if msg.err != nil {
				m.trackErr = msg.err.Error()
				m.trackPoll = false
			} else {
				m.trackTrade = msg.t
				// Keep polling until the trade reaches a terminal state.
				if msg.t != nil && !isTerminal(msg.t.Status) {
					m.trackPoll = true
				} else {
					m.trackPoll = false
				}
			}
			return m, nil
		}
		if msg.err == nil && msg.t != nil {
			m.trade = msg.t
		}
		return m, nil

	case clearToastMsg:
		m.copyToast = ""
		return m, nil

	case tickMsg:
		var cmds []tea.Cmd
		if m.pollOn && m.trade != nil && m.trade.ID != "" && !isTerminal(m.trade.Status) {
			cmds = append(cmds, m.cmdStatus(m.trade.ID, false))
		}
		if m.trackPoll && m.trackTrade != nil && m.trackTrade.ID != "" && !isTerminal(m.trackTrade.Status) {
			cmds = append(cmds, m.cmdStatus(m.trackTrade.ID, true))
		}
		if len(cmds) > 0 {
			cmds = append(cmds, tickCmd())
			return m, tea.Batch(cmds...)
		}
		// Restart the tick if either pollers are still on but waiting for
		// a fresh status; otherwise the tick chain dies and never resumes.
		if m.pollOn || m.trackPoll {
			return m, tickCmd()
		}
		return m, nil
	}
	return m, nil
}

// isTypingState returns true when the active step receives raw text
// input (so we don't intercept letters as global tab hotkeys).
func (m Model) isTypingState() bool {
	if m.tab == tabTrack {
		return m.trackIn.Focused()
	}
	if m.tab != tabSwap && m.tab != tabGhost {
		return false
	}
	switch m.state {
	case stPickFrom, stPickTo, stAmount, stAddress, stMemo:
		// Pickers also accept free-text tickers like 'LTC' / 'TRX' /
		// 'TRC20' — so global hotkeys ('s'/'t'/'a') must be disabled
		// while the user is typing. Tabs are still reachable via
		// click or Tab key.
		return true
	}
	return false
}

// handleDepositKeys handles the keys that act on the deposit panel —
// arrow nav, enter-to-copy, q (fullscreen QR), g (image mode), c/C
// (direct copy shortcuts). Runs before tab-specific dispatch so it
// works in both stOrdered (Swap tab) and Track tab. Returns
// handled=true when the keystroke was consumed.
func (m Model) handleDepositKeys(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	var activeAddr string
	if (m.tab == tabSwap || m.tab == tabGhost) && m.state == stOrdered && m.trade != nil {
		activeAddr = m.trade.DepositAddress
	} else if m.tab == tabTrack && m.trackTrade != nil && !isTerminal(m.trackTrade.Status) {
		activeAddr = m.trackTrade.DepositAddress
	}
	if activeAddr == "" {
		return m, nil, false
	}
	switch msg.String() {
	case "q":
		m.qrFullScreen = !m.qrFullScreen
		return m, nil, true
	case "i":
		m.qrImageMode = !m.qrImageMode
		return m, nil, true
	case "up", "k":
		if m.depositFocus > 0 {
			m.depositFocus--
		}
		return m, nil, true
	case "down", "j":
		if m.depositFocus < 1 {
			m.depositFocus++
		}
		return m, nil, true
	case "enter":
		var label string
		if m.depositFocus == 0 {
			label = "📋 address copied to clipboard"
			m.copyText(activeAddr)
		} else {
			label = m.openBrowser(qrBrowserURL(activeAddr))
		}
		m.copyToast = label
		return m, tea.Tick(2*time.Second, func(_ time.Time) tea.Msg { return clearToastMsg{} }), true
	case "c":
		m.copyText(activeAddr)
		m.copyToast = "📋 address copied to clipboard"
		return m, tea.Tick(2*time.Second, func(_ time.Time) tea.Msg { return clearToastMsg{} }), true
	case "C":
		m.copyText(qrBrowserURL(activeAddr))
		m.copyToast = "📋 QR URL copied to clipboard"
		return m, tea.Tick(2*time.Second, func(_ time.Time) tea.Msg { return clearToastMsg{} }), true
	}
	return m, nil, false
}

// writeClipboard emits the OSC 52 clipboard-write escape directly to the
// program's output writer (mutex-protected so it doesn't race with
// bubbletea's renderer). View() never sees the escape.
func (m Model) copyText(text string) {
	if text == "" {
		return
	}
	if m.cfg.LocalActions && writeLocalClipboard(text) == nil {
		return
	}
	m.writeOSC52(text)
}

func (m Model) openBrowser(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	if m.cfg.LocalActions && openLocalBrowser(rawURL) == nil {
		return "↗ opened QR in browser"
	}
	m.writeOSC52(rawURL)
	return "📋 QR URL copied — paste into browser"
}

func (m Model) writeOSC52(text string) {
	if m.cfg.ClipboardWriter != nil && text != "" {
		_, _ = m.cfg.ClipboardWriter.Write([]byte(osc52Clipboard(text)))
	}
}

func (m Model) updateSwap(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		// step back through the wizard
		switch m.state {
		case stPickTo:
			m.state = stPickFrom
		case stAmount:
			m.state = stPickTo
		case stAddress:
			m.state = stAmount
		case stMemo:
			m.state = stAddress
		case stQuoted, stError:
			// Back to last input step
			m.state = stAddress
			m.swapErr = ""
		case stOrdered:
			if m.qrFullScreen {
				m.qrFullScreen = false
				return m, nil
			}
			// reset whole wizard
			m.resetSwap()
		}
		return m, nil
	}

	switch m.state {
	case stPickFrom, stPickTo:
		return m.updatePicker(msg)
	case stAmount:
		return m.updateAmount(msg)
	case stAddress:
		return m.updateAddress(msg)
	case stMemo:
		return m.updateMemo(msg)
	case stQuoted:
		key := msg.String()
		// Bucket selection: 1-4 number-pick, tab/down cycle forward, shift+tab/up cycle back.
		if len(key) == 1 && key >= "1" && key <= "9" {
			modes := m.picks.modes()
			idx := int(key[0]-'0') - 1
			if idx >= 0 && idx < len(modes) {
				m.routePick = modes[idx]
			}
			return m, nil
		}
		if key == "tab" || key == "down" || key == "right" {
			m.routePick = cycleMode(m.picks.modes(), m.routePick, +1)
			return m, nil
		}
		if key == "shift+tab" || key == "up" || key == "left" {
			m.routePick = cycleMode(m.picks.modes(), m.routePick, -1)
			return m, nil
		}
		if key == "enter" {
			if m.cfg.DryRun {
				// Stop here. Show a friendly explainer in the error
				// channel (no actual error — just preempts the call).
				m.swapErr = "dry-run mode — POST /create suppressed. esc to start over."
				m.state = stError
				return m, nil
			}
			m.state = stCreating
			return m, m.cmdCreate()
		}
	case stError:
		if msg.String() == "enter" {
			m.swapErr = ""
			m.state = stAddress
		}
	}
	return m, nil
}

func (m *Model) resetSwap() {
	m.state = stPickFrom
	m.from = ""
	m.to = ""
	m.amtIn.SetValue("")
	m.addrIn.SetValue("")
	m.memoIn.SetValue("")
	m.quote = nil
	m.picks = routePicks{}
	m.routePick = ""
	m.trade = nil
	m.swapErr = ""
	m.pollOn = false
	m.qrFullScreen = false
	m.qrImageMode = false
	m.copyToast = ""
	m.depositFocus = 0
}

// updatePicker handles stPickFrom / stPickTo: digits 1-9 pick from the
// numbered list; any other typing is interpreted as a free-text ticker
// followed by Enter.
func (m Model) updatePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	// Digit shortcut
	if len(key) == 1 && key >= "1" && key <= "9" {
		idx := int(key[0]-'0') - 1
		if idx >= 0 && idx < len(topAssets) {
			m.assignAsset(topAssets[idx])
			return m, nil
		}
	}
	// Free-text typing: keep an input visible at the bottom.
	// Reuse amtIn as a temporary scratch input — too much state otherwise.
	switch key {
	case "enter":
		txt := strings.TrimSpace(m.pickerScratch())
		if txt == "" {
			return m, nil
		}
		m.assignAsset(strings.ToUpper(txt))
		m.setPickerScratch("")
		return m, nil
	case "backspace":
		s := m.pickerScratch()
		if len(s) > 0 {
			m.setPickerScratch(s[:len(s)-1])
		}
		return m, nil
	}
	if len(key) == 1 {
		m.setPickerScratch(m.pickerScratch() + key)
	}
	return m, nil
}

// assignAsset commits the picked ticker to the current step and advances.
func (m *Model) assignAsset(t string) {
	t = strings.ToUpper(strings.TrimSpace(t))
	if m.state == stPickFrom {
		m.from = t
		m.state = stPickTo
		return
	}
	if m.state == stPickTo {
		m.to = t
		m.state = stAmount
		m.amtIn.Focus()
	}
}

func (m Model) updateAmount(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" {
		amt, err := strconv.ParseFloat(strings.TrimSpace(m.amtIn.Value()), 64)
		if err != nil || amt <= 0 {
			m.swapErr = "amount must be a positive number"
			return m, nil
		}
		m.amtIn.Blur()
		m.state = stAddress
		m.addrIn.Focus()
		m.swapErr = ""
		return m, nil
	}
	var cmd tea.Cmd
	m.amtIn, cmd = m.amtIn.Update(msg)
	return m, cmd
}

func (m Model) updateAddress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" {
		addr := strings.TrimSpace(m.addrIn.Value())
		if addr == "" {
			m.swapErr = "destination address required"
			return m, nil
		}
		// Pre-flight format heuristic — catches typos before sending
		// the order to upstream engines (which may take real $$ to
		// learn the address was malformed).
		toTicker, toNet := splitTickerNet(m.to)
		if ok, hint := validateAddress(toTicker, toNet, addr); !ok {
			m.swapErr = hint
			return m, nil
		}
		m.addrIn.Blur()
		// Memo is requested upfront for everything; we always show the step
		// but blank-and-Enter skips it cleanly.
		m.state = stMemo
		m.memoIn.Focus()
		m.swapErr = ""
		return m, nil
	}
	var cmd tea.Cmd
	m.addrIn, cmd = m.addrIn.Update(msg)
	return m, cmd
}

func (m Model) updateMemo(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" {
		m.memoIn.Blur()
		m.state = stQuoting
		return m, m.cmdEstimate()
	}
	var cmd tea.Cmd
	m.memoIn, cmd = m.memoIn.Update(msg)
	return m, cmd
}

func (m Model) updateTrack(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		id := strings.TrimSpace(m.trackIn.Value())
		if id == "" {
			return m, nil
		}
		m.trackBusy = true
		m.trackErr = ""
		m.trackTrade = nil
		// Kick the first lookup, plus a tick so the auto-poll loop picks
		// up once we get the trade back.
		return m, tea.Batch(m.cmdStatus(id, true), tickCmd())
	case "esc":
		m.trackTrade = nil
		m.trackErr = ""
		m.trackPoll = false
		m.trackIn.SetValue("")
		// Clear deposit-panel state too — otherwise fullscreen / focus /
		// toast / image-mode bleed into the next tracked order.
		m.qrFullScreen = false
		m.qrImageMode = false
		m.copyToast = ""
		m.depositFocus = 0
		return m, nil
	}
	var cmd tea.Cmd
	m.trackIn, cmd = m.trackIn.Update(msg)
	return m, cmd
}

// --- picker scratch (free-text typing buffer for asset pickers) ---

func (m *Model) pickerScratch() string {
	if m.state == stPickFrom {
		return m.from // reuse field as buffer
	}
	return m.to
}
func (m *Model) setPickerScratch(s string) {
	if m.state == stPickFrom {
		m.from = s
	} else {
		m.to = s
	}
}

// --- helpers ---

func splitTickerNet(in string) (string, string) {
	in = strings.ToUpper(strings.TrimSpace(in))
	for _, sep := range []string{"-", "/", ":"} {
		if i := strings.Index(in, sep); i > 0 {
			return in[:i], in[i+1:]
		}
	}
	return in, ""
}

func isTerminal(status string) bool {
	switch strings.ToUpper(status) {
	case "FINISHED", "FAILED", "EXPIRED", "REFUNDED":
		return true
	}
	return false
}

// fmtAmt renders a crypto amount with magnitude-aware precision so the
// route-card line stays on one row. Float64 round-trip precision (the
// previous `-1` flag) emitted 17 digits for values like 0.03507851359904113,
// which wrapped the card and broke the box-drawing border. Cap precision
// by magnitude, then strip trailing zeros so neat values print short.
func fmtAmt(n float64) string {
	if n == 0 {
		return "—"
	}
	abs := math.Abs(n)
	var prec int
	switch {
	case abs >= 1000:
		prec = 2
	case abs >= 1:
		prec = 4
	case abs >= 0.01:
		prec = 6
	case abs >= 0.0001:
		prec = 8
	default:
		prec = 10
	}
	s := strconv.FormatFloat(n, 'f', prec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

// --- view ---

func (m Model) View() string {
	if m.width == 0 {
		return ""
	}
	return m.viewBody()
}

func (m Model) viewBody() string {
	// Fullscreen QR bypasses the card / header / hint layout entirely so
	// the QR has no horizontal width constraint and no padding/border
	// processing that could mangle its multi-line BG-painted rows.
	if m.qrFullScreen {
		var addr string
		if m.tab == tabSwap && m.trade != nil {
			addr = m.trade.DepositAddress
		} else if m.tab == tabTrack && m.trackTrade != nil {
			addr = m.trackTrade.DepositAddress
		}
		if addr != "" {
			if m.qrImageMode {
				body := renderQRImage(addr) +
					"\n\n" + styleOk.Render(addr) +
					"\n" + styleDim.Render("q exit · g toggle image/text mode")
				return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, body)
			}
			// Text-mode QR: emit straight from renderQRFullScreen with
			// per-row padding to m.width. Skip lipgloss.Place — Place pads
			// each line with default-BG spaces and Bubble Tea would still
			// suffix `[K`, painting bands between rows.
			return m.renderQRFullScreen(addr)
		}
	}
	header := m.renderHeader()
	var body string
	switch m.tab {
	case tabSwap, tabGhost:
		body = m.renderSwap()
	case tabTrack:
		body = m.renderTrack()
	case tabAbout:
		body = m.renderAbout()
	}
	hint := m.renderHint()

	// Centered card layout.
	card := lipgloss.JoinVertical(lipgloss.Left, header, "", body)
	cardBox := styleCard.Render(card)

	// Stack: card centered + hint bar at bottom
	stack := lipgloss.JoinVertical(lipgloss.Center, cardBox, "", styleDim.Render(hint))
	placed := lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, stack)
	// Scan registers the rendered zone bounds so MouseMsg.InBounds() works.
	return m.zm.Scan(placed)
}

func (m Model) renderHeader() string {
	user := m.cfg.Username
	if user == "" {
		user = "kyc.rip"
	}
	left := styleUser.Render(user + "@swap")
	tabs := []string{
		m.zm.Mark(zTabSwap, tabRender("Swap", m.tab == tabSwap)),
		m.zm.Mark(zTabGhost, tabRender("Ghost", m.tab == tabGhost)),
		m.zm.Mark(zTabTrack, tabRender("Track", m.tab == tabTrack)),
	}
	right := strings.Join(tabs, "  ")
	// Spacer flex
	spacerWidth := cardInnerWidth - lipgloss.Width(left) - lipgloss.Width(right)
	if spacerWidth < 1 {
		spacerWidth = 1
	}
	spacer := strings.Repeat(" ", spacerWidth)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, spacer, right)
}

// tabRender wraps each tab in its own zone so mouse clicks resolve to it.
// We don't have access to the zone manager here so the caller wraps after.
func tabRender(name string, active bool) string {
	if active {
		return styleTabActive.Render(name)
	}
	return styleTabIdle.Render(name)
}

func (m Model) renderSwap() string {
	body := m.renderSwapBody()
	if m.ghostMode {
		banner := styleGhostBanner.Render("[GHOST]  privacy-routed  ·  no provider sees full path")
		body = banner + "\n\n" + body
	}
	return body
}

func (m Model) renderSwapBody() string {
	switch m.state {
	case stPickFrom:
		return m.renderPicker("Sending", m.from)
	case stPickTo:
		return m.renderPicker("Receiving", m.to)
	case stAmount:
		return m.renderAmount()
	case stAddress:
		return m.renderAddress()
	case stMemo:
		return m.renderMemo()
	case stQuoting:
		if m.ghostMode {
			return styleDim.Render("fetching ghost-bridge quote across privacy engines…")
		}
		return styleDim.Render("fetching best quote across engines…")
	case stQuoted:
		return m.renderQuoted()
	case stCreating:
		return styleDim.Render("creating order…")
	case stOrdered:
		return m.renderOrdered()
	case stError:
		return styleErr.Render("error: ") + m.swapErr + "\n\n" + styleDim.Render("press enter to retry · esc to step back")
	}
	return ""
}

func (m Model) renderPicker(label, scratch string) string {
	var rows []string
	rows = append(rows, styleAccent.Render(label+":")+" "+styleDim.Render("pick a number 1-9, click a row, or type a ticker"))
	rows = append(rows, "")
	for i, a := range topAssets {
		if i >= 9 {
			break
		}
		row := fmt.Sprintf("  %s  %s", styleWarn.Render(strconv.Itoa(i+1)+"."), a)
		rows = append(rows, m.zm.Mark(zAssetRow+strconv.Itoa(i), row))
	}
	rows = append(rows, "")
	rows = append(rows, styleDim.Render("type:")+" "+styleField.Render(padInput(scratch, 28)))
	rows = append(rows, "")
	rows = append(rows, m.zm.Mark(zButton, styleButton.Render("[ Enter to confirm ]")))
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m Model) renderAmount() string {
	rows := []string{
		styleAccent.Render("Sending: ") + m.from,
		styleDim.Render("amount you send"),
		"",
		styleFieldActive.Render(padInput(m.amtIn.View(), 24)),
	}
	if m.swapErr != "" {
		rows = append(rows, "", styleErr.Render(m.swapErr))
	}
	rows = append(rows, "", m.zm.Mark(zButton, styleButton.Render("[ Continue ]")))
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m Model) renderAddress() string {
	rows := []string{
		styleAccent.Render("Receiving: ") + m.to,
		styleDim.Render("destination wallet address"),
		"",
		styleFieldActive.Render(padInput(m.addrIn.View(), 60)),
	}
	if m.swapErr != "" {
		rows = append(rows, "", styleErr.Render(m.swapErr))
	}
	rows = append(rows, "", m.zm.Mark(zButton, styleButton.Render("[ Continue ]")))
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m Model) renderMemo() string {
	rows := []string{
		styleAccent.Render("Memo / dest tag"),
		styleDim.Render("optional · leave blank and press Enter to skip"),
		"",
		styleFieldActive.Render(padInput(m.memoIn.View(), 30)),
		"",
		m.zm.Mark(zButton, styleButton.Render("[ Get quote ]")),
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m Model) renderQuoted() string {
	q := m.quote
	if q == nil {
		return ""
	}
	from, _ := splitTickerNet(m.from)
	to, _ := splitTickerNet(m.to)

	rows := []string{
		styleAccent.Render("Sending:   ") + fmtAmt(q.AmountFrom) + " " + from,
		styleAccent.Render("Pick a route — same buckets as the bot"),
		"",
	}

	modes := m.picks.modes()
	for i, mode := range modes {
		r := m.picks.get(mode)
		num := fmt.Sprintf("%d.", i+1)
		head := fmt.Sprintf("%s  %s %s", num, mode.Glyph(), mode.Label())
		var body string
		if m.ghostMode && r.BridgeLabel != "" {
			// Ghost-mode card emphasises the bridge label (e.g. MONERO_TUNNEL,
			// ZANO_PRIVACY_BRIDGE, FROST) over the raw provider name — that's
			// what the user is actually trusting in a privacy route.
			body = fmt.Sprintf("   %s · ~%s %s · ETA %dm · %s",
				r.BridgeLabel, fmtAmt(r.AmountTo), to, r.ETA, badgeOrDash(r.BridgeBadge))
		} else {
			body = fmt.Sprintf("   %s · ~%s %s · ETA %dm · KYC %s",
				r.Provider, fmtAmt(r.AmountTo), to, r.ETA, ratingOrDash(r.KYC))
		}
		card := lipgloss.JoinVertical(lipgloss.Left, head, body)
		if mode == m.routePick {
			card = styleRouteCardActive.Render(card)
		} else {
			card = styleRouteCard.Render(card)
		}
		rows = append(rows, card)
	}

	rows = append(rows, "",
		styleDim.Render(fmt.Sprintf("1 %s = %s %s", from, fmtAmt(q.Rate), to)),
		"",
		m.zm.Mark(zButton, styleButton.Render("[ Confirm — create order ]")),
		"",
		styleDim.Render("1-4 pick · tab cycle · enter confirm · esc back"),
	)
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func badgeOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ToUpper(s)
}

func ratingOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ToUpper(s)
}

func (m Model) renderOrdered() string {
	t := m.trade
	if t == nil {
		return ""
	}
	if m.qrFullScreen {
		return m.renderQRFullScreen(t.DepositAddress)
	}
	depositURI := walletURI(t.FromTicker, t.FromNetwork, t.DepositAddress, t.FromAmount)
	qrURL := qrBrowserURL(t.DepositAddress)
	qrLink := osc8(qrURL, styleAccent.Render("[ open QR in browser ]"))
	addrLine := osc8(depositURI, styleOk.Render(t.DepositAddress))
	addrCaret, qrCaret := caretFor(m.depositFocus, 0), caretFor(m.depositFocus, 1)
	left := []string{
		styleAccent.Render("Order ") + t.ID,
		styleAccent.Render("Status: ") + styleOk.Render(strings.ToUpper(t.Status)),
		"",
		styleDim.Render("Send"),
		styleOk.Render(fmt.Sprintf("%s %s", fmtAmt(t.FromAmount), strings.ToUpper(t.FromTicker))),
		"",
		styleDim.Render("To deposit address  ") + styleDim.Render("(↑↓ move · enter copy/open · c copy address · C copy QR URL · ctrl+s select)"),
		addrCaret + addrLine,
		"",
		qrCaret + qrLink,
	}
	if m.copyToast != "" {
		left = append(left, "", styleOk.Render(m.copyToast))
	}
	if t.DepositMemo != "" {
		left = append(left, "", styleDim.Render("Memo (REQUIRED)"), styleErr.Render(t.DepositMemo))
	}
	left = append(left, "",
		styleDim.Render(fmt.Sprintf("Receive ~%s %s → %s", fmtAmt(t.ToAmount), strings.ToUpper(t.ToTicker), t.AddressUser)),
		"",
		styleDim.Render("auto-refresh every 5s · q full-size QR · esc reset"),
	)
	return lipgloss.JoinVertical(lipgloss.Left, left...)
}

func (m Model) renderTrack() string {
	rows := []string{
		styleAccent.Render("Track"),
		styleDim.Render("paste a trade id and press Enter"),
		"",
		styleFieldActive.Render(padInput(m.trackIn.View(), 40)),
	}
	if m.trackBusy {
		rows = append(rows, "", styleDim.Render("looking up…"))
	} else if m.trackErr != "" {
		rows = append(rows, "", styleErr.Render("error: "+m.trackErr))
	} else if m.trackTrade != nil {
		t := m.trackTrade
		rows = append(rows, "",
			styleAccent.Render("Status: ")+styleOk.Render(strings.ToUpper(t.Status)),
			styleDim.Render(fmt.Sprintf("send %s %s", fmtAmt(t.FromAmount), strings.ToUpper(t.FromTicker))),
			styleDim.Render(fmt.Sprintf("recv %s %s → %s", fmtAmt(t.ToAmount), strings.ToUpper(t.ToTicker), t.AddressUser)),
		)
		// Show deposit address + QR for orders that still need funding —
		// users who Tracked an old order id should see where to send.
		showDeposit := !isTerminal(t.Status) && t.DepositAddress != ""
		if showDeposit {
			if m.qrFullScreen {
				return m.renderQRFullScreen(t.DepositAddress)
			}
			depositURI := walletURI(t.FromTicker, t.FromNetwork, t.DepositAddress, t.FromAmount)
			qrURL := qrBrowserURL(t.DepositAddress)
			qrLink := osc8(qrURL, styleAccent.Render("[ open QR in browser ]"))
			addrLine := osc8(depositURI, styleOk.Render(t.DepositAddress))
			addrCaret, qrCaret := caretFor(m.depositFocus, 0), caretFor(m.depositFocus, 1)
			rows = append(rows, "",
				styleDim.Render("To deposit address  ")+styleDim.Render("(↑↓ move · enter copy)"),
				addrCaret+addrLine,
				"",
				qrCaret+qrLink,
			)
			if m.copyToast != "" {
				rows = append(rows, "", styleOk.Render(m.copyToast))
			}
			if t.DepositMemo != "" {
				rows = append(rows, "", styleDim.Render("Memo (REQUIRED)"), styleErr.Render(t.DepositMemo))
			}
		}
		rows = append(rows, "",
			styleDim.Render("txIn:  "+t.TxIn),
			styleDim.Render("txOut: "+t.TxOut),
		)
		if m.trackPoll {
			rows = append(rows, "", styleDim.Render("auto-refresh every 5s · q full-size QR · esc clear"))
		}
		leftBlock := lipgloss.JoinVertical(lipgloss.Left, rows...)
		return leftBlock
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// caretFor returns "▸ " when the focus index matches the row index, "  "
// otherwise. Used to mark the keyboard-focused row in the deposit panel.
func caretFor(focus, row int) string {
	if focus == row {
		return styleWarn.Render("▸ ")
	}
	return "  "
}

// renderQRFullScreen returns the QR alone, padded to exactly m.width per
// row so Bubble Tea's standard renderer skips its `ESC[K` (erase line
// right) suffix. That suffix was painting the default terminal BG into
// the line trailer — visible as dark horizontal bands between every QR
// row. Padding to terminal width makes `ansi.StringWidth(line) >=
// r.width` so the renderer's gate (line < width → append [K) never
// triggers. Diagnosed by Codex post-mortem.
func (m Model) renderQRFullScreen(addr string) string {
	qr := strings.TrimRight(renderQR(addr), "\n")
	if qr == "" {
		return styleDim.Render("(no QR available — copy the address text above)")
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	pad := func(line string) string {
		w := ansi.StringWidth(line)
		if w >= width {
			return line
		}
		need := width - w
		left := need / 2
		right := need - left
		return strings.Repeat(" ", left) + line + strings.Repeat(" ", right)
	}
	var b strings.Builder
	for i, line := range strings.Split(qr, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(pad(line))
	}
	b.WriteString("\n")
	b.WriteString(pad(""))
	b.WriteString("\n")
	b.WriteString(pad(styleOk.Render(addr)))
	b.WriteString("\n")
	b.WriteString(pad(styleDim.Render("q · esc — back to order")))
	return b.String()
}

func (m Model) renderAbout() string {
	fp := m.cfg.Fingerprint
	if fp == "" {
		fp = "(local CLI · no host key)"
	}
	rows := []string{
		styleAccent.Render("kyc.rip · terminal-only swap"),
		"",
		styleDim.Render("Privacy-first crypto swap aggregator,"),
		styleDim.Render("served over SSH. No JS, no cookies,"),
		styleDim.Render("no browser fingerprint."),
		"",
		styleAccent.Render("Channels"),
		"  clearnet  ssh swap.kyc.rip",
		"  https     https://swap.kyc.rip  (landing only)",
		"  tor       torsocks ssh kyccli2b6y3iwxhkpoetzf",
		"            yozmmrwipaakznyvhgl7a264l7tflvzqad.onion",
		"  i2p       ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:4447 %h %p' \\",
		"               kyccliymrfjyumorhpfujsqfvbb2vakaj",
		"               klhw5kfomec7umwhpgq.b32.i2p",
		"",
		styleAccent.Render("Verify host key before connecting"),
		"  " + fp,
		"",
		styleAccent.Render("Source"),
		"  github.com/kyc-rip/cli",
		"",
		styleDim.Render("press s · t · ctrl+c"),
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m Model) renderHint() string {
	switch m.tab {
	case tabSwap, tabGhost:
		switch m.state {
		case stPickFrom, stPickTo:
			return "1-9 pick · type ticker · enter confirm · s swap · g ghost · t track · a about · ctrl+c quit"
		case stAmount, stAddress, stMemo:
			return "type · enter continue · esc back · ctrl+c quit"
		case stQuoted:
			return "enter confirm · esc back · t track · ctrl+c quit"
		case stOrdered:
			return "esc reset · t track · ctrl+c quit"
		}
	case tabTrack:
		return "type id · enter lookup · esc clear · s swap · g ghost · a about · ctrl+c quit"
	case tabAbout:
		return "esc/enter back · s swap · g ghost · t track · ctrl+c quit"
	}
	return "ctrl+c quit"
}

// padInput right-pads s to width w (preserving lipgloss-rendered width
// where possible) so input fields don't reflow when text is empty.
func padInput(s string, w int) string {
	cur := lipgloss.Width(s)
	if cur >= w {
		return s
	}
	return s + strings.Repeat(" ", w-cur)
}
