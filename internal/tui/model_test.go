package tui

import (
	"bytes"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kyc-rip/cli/internal/api"
)

func TestSplitTickerNet(t *testing.T) {
	cases := []struct {
		in          string
		ticker, net string
	}{
		{"btc", "BTC", ""},
		{"USDT-TRC20", "USDT", "TRC20"},
		{"usdt/erc20", "USDT", "ERC20"},
		{"eth:base", "ETH", "BASE"},
		{"  BTC  ", "BTC", ""},
	}
	for _, tc := range cases {
		gt, gn := splitTickerNet(tc.in)
		if gt != tc.ticker || gn != tc.net {
			t.Errorf("splitTickerNet(%q) = (%q,%q), want (%q,%q)", tc.in, gt, gn, tc.ticker, tc.net)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	cases := map[string]bool{
		"WAITING":    false,
		"finished":   true,
		"FINISHED":   true,
		"FAILED":     true,
		"EXPIRED":    true,
		"REFUNDED":   true,
		"EXCHANGING": false,
	}
	for k, want := range cases {
		if got := isTerminal(k); got != want {
			t.Errorf("isTerminal(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestOrderedTradeIDCopyShortcut(t *testing.T) {
	var out bytes.Buffer
	m := New(Config{ClipboardWriter: NewLockedWriter(&out)})
	m.tab = tabSwap
	m.state = stOrdered
	m.trade = &api.Trade{
		ID:             "TRADE123",
		Status:         "WAITING",
		DepositAddress: "deposit-address",
	}

	got, cmd, handled := m.handleDepositKeys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !handled {
		t.Fatal("y key was not handled")
	}
	if cmd == nil {
		t.Fatal("expected toast clear command")
	}
	if got.copyToast != "📋 trade ID copied to clipboard" {
		t.Fatalf("copyToast = %q", got.copyToast)
	}
	if !strings.Contains(out.String(), osc52Clipboard("TRADE123")) {
		t.Fatalf("clipboard output %q does not contain trade ID OSC52", out.String())
	}
}

func TestOrderedViewDocumentsTradeIDCopyShortcut(t *testing.T) {
	m := New(Config{})
	m.tab = tabSwap
	m.state = stOrdered
	m.trade = &api.Trade{
		ID:             "TRADE123",
		Status:         "WAITING",
		DepositAddress: "deposit-address",
	}

	view := m.renderOrdered()
	if !strings.Contains(view, "y copy trade ID") {
		t.Fatalf("ordered view does not document trade ID shortcut: %q", view)
	}
}
