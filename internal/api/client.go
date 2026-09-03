package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBase    = "https://api.kyc.rip"
	defaultTimeout = 12 * time.Second
)

type Client struct {
	base    string
	apiKey  string
	timeout time.Duration
	http    *http.Client
}

type Option func(*Client)

func WithBase(s string) Option           { return func(c *Client) { c.base = strings.TrimRight(s, "/") } }
func WithAPIKey(k string) Option         { return func(c *Client) { c.apiKey = k } }
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

func New(opts ...Option) *Client {
	c := &Client{base: defaultBase, timeout: defaultTimeout}
	for _, o := range opts {
		o(c)
	}
	c.http = &http.Client{Timeout: c.timeout}
	return c
}

type Currency struct {
	ID       string   `json:"id"`
	Ticker   string   `json:"ticker"`
	Network  string   `json:"network"`
	Name     string   `json:"name"`
	Image    string   `json:"image,omitempty"`
	Minimum  float64  `json:"minimum,omitempty"`
	Maximum  float64  `json:"maximum,omitempty"`
	Memo     bool     `json:"memo,omitempty"`
	Engine   string   `json:"engine,omitempty"`
	Engines  []string `json:"engines,omitempty"`
	Priority int      `json:"priority,omitempty"`
}

type Route struct {
	Provider     string  `json:"provider"`
	Engine       string  `json:"engine"`
	AmountTo     float64 `json:"amount_to"`
	AmountFrom   float64 `json:"amount_from"`
	KYC          string  `json:"kyc"`
	LogPolicy    string  `json:"log_policy"`
	ETA          int     `json:"eta"`
	Fixed        bool    `json:"fixed"`
	Spread       float64 `json:"spread,omitempty"`
	HoudiniQuote any     `json:"_houdiniQuote,omitempty"`
	// Bridge / Ghost-only metadata. Populated by /v2/exchange/bridge/estimate.
	BridgeLabel     string `json:"bridgeLabel,omitempty"`
	BridgeBadge     string `json:"bridgeBadge,omitempty"`
	BridgeHighlight string `json:"bridgeHighlight,omitempty"`
	RequiresRefund  bool   `json:"requiresRefund,omitempty"`
}

type Estimate struct {
	ID         string  `json:"id"`
	Rate       float64 `json:"rate"`
	AmountFrom float64 `json:"amount_from"`
	AmountTo   float64 `json:"amount_to"`
	Min        float64 `json:"min"`
	Max        float64 `json:"max"`
	Provider   string  `json:"provider"`
	Engine     string  `json:"engine"`
	KYCRating  string  `json:"kyc_rating"`
	ETA        int     `json:"eta"`
	Routes     []Route `json:"routes"`
}

type CreateReq struct {
	TradeID       string  `json:"trade_id,omitempty"`
	Provider      string  `json:"provider"`
	Engine        string  `json:"engine,omitempty"`
	FromCurrency  string  `json:"from_currency"`
	ToCurrency    string  `json:"to_currency"`
	FromNetwork   string  `json:"from_network,omitempty"`
	ToNetwork     string  `json:"to_network,omitempty"`
	AmountFrom    float64 `json:"amount_from,omitempty"`
	AmountTo      float64 `json:"amount_to,omitempty"`
	AddressTo     string  `json:"address_to"`
	FixedRate     bool    `json:"fixed_rate,omitempty"`
	HoudiniQuote  any     `json:"_houdiniQuote,omitempty"`
	AddressMemo   string  `json:"address_memo,omitempty"`
	RefundAddress string  `json:"refund_address,omitempty"`
	Source        string  `json:"source,omitempty"`
}

type Trade struct {
	ID             string  `json:"id"`
	TradeID        string  `json:"trade_id,omitempty"`
	Status         string  `json:"status"`
	Engine         string  `json:"engine"`
	Provider       string  `json:"provider"`
	FromTicker     string  `json:"fromTicker"`
	FromNetwork    string  `json:"fromNetwork"`
	FromAmount     float64 `json:"fromAmount"`
	ToTicker       string  `json:"toTicker"`
	ToNetwork      string  `json:"toNetwork"`
	ToAmount       float64 `json:"toAmount"`
	DepositAddress string  `json:"depositAddress"`
	DepositMemo    string  `json:"depositMemo,omitempty"`
	AddressUser    string  `json:"address_user"`
	TxIn           string  `json:"txIn,omitempty"`
	TxOut          string  `json:"txOut,omitempty"`
	OrderURL       string  `json:"orderUrl,omitempty"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	u := c.base + path
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kyc-cli/0.1 (+https://kyc.rip)")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("api %s %s: %d %s", method, path, resp.StatusCode, truncate(string(rb), 200))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rb, out)
}

func (c *Client) Currencies(ctx context.Context) ([]Currency, error) {
	var out []Currency
	if err := c.do(ctx, http.MethodGet, "/v2/exchange/currencies", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Estimate(ctx context.Context, from, fromNet, to, toNet string, amount float64) (*Estimate, error) {
	q := url.Values{}
	q.Set("from", from)
	q.Set("to", to)
	if fromNet != "" {
		q.Set("from_net", fromNet)
		q.Set("network_from", fromNet)
	}
	if toNet != "" {
		q.Set("to_net", toNet)
		q.Set("network_to", toNet)
	}
	q.Set("amount", strconv.FormatFloat(amount, 'f', -1, 64))
	q.Set("type", "from")
	var out Estimate
	if err := c.do(ctx, http.MethodGet, "/v2/exchange/estimate?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Create(ctx context.Context, req CreateReq) (*Trade, error) {
	if req.Source == "" {
		req.Source = "cli"
	}
	if err := validateCreateReq(req); err != nil {
		return nil, err
	}
	var out Trade
	if err := c.do(ctx, http.MethodPost, "/v2/exchange/create", req, &out); err != nil {
		return nil, err
	}
	if out.ID == "" && out.TradeID != "" {
		out.ID = out.TradeID
	}
	return &out, nil
}

// EstimateBridge calls the Ghost / privacy-routed estimate endpoint. The
// response shape matches the regular Estimate but each Route carries
// extra bridge metadata (BridgeLabel, BridgeBadge, BridgeHighlight).
func (c *Client) EstimateBridge(ctx context.Context, from, fromNet, to, toNet string, amount float64) (*Estimate, error) {
	q := url.Values{}
	q.Set("from", from)
	q.Set("to", to)
	if fromNet != "" {
		q.Set("network_from", fromNet)
	}
	if toNet != "" {
		q.Set("network_to", toNet)
	}
	q.Set("amount", strconv.FormatFloat(amount, 'f', -1, 64))
	var out Estimate
	if err := c.do(ctx, http.MethodGet, "/v2/exchange/bridge/estimate?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateBridge calls the Ghost / privacy-routed create endpoint. Some
// engines (e.g. Zano) require a refund_address; the API rejects without
// it. The response can be a single Trade or an array of legs — we
// unmarshal as a generic and pick the first leg as the user-facing trade.
func (c *Client) CreateBridge(ctx context.Context, req CreateReq) (*Trade, error) {
	if req.Source == "" {
		req.Source = "cli"
	}
	if err := validateCreateReq(req); err != nil {
		return nil, err
	}
	// Bridge create may return either a Trade object or [Trade, Trade...]
	// depending on whether the route is multi-leg. Unmarshal into json.RawMessage
	// and disambiguate.
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, "/v2/exchange/bridge/create", req, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty bridge create response")
	}
	if raw[0] == '[' {
		var trades []Trade
		if err := json.Unmarshal(raw, &trades); err != nil {
			return nil, err
		}
		if len(trades) == 0 {
			return nil, fmt.Errorf("empty bridge trades array")
		}
		t := trades[0]
		if t.ID == "" && t.TradeID != "" {
			t.ID = t.TradeID
		}
		return &t, nil
	}
	var out Trade
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.ID == "" && out.TradeID != "" {
		out.ID = out.TradeID
	}
	return &out, nil
}

func (c *Client) Status(ctx context.Context, id string) (*Trade, error) {
	var out Trade
	if err := c.do(ctx, http.MethodGet, "/v2/exchange/status/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func validateCreateReq(req CreateReq) error {
	missing := []string{}
	if strings.TrimSpace(req.Provider) == "" {
		missing = append(missing, "provider")
	}
	if strings.TrimSpace(req.FromCurrency) == "" {
		missing = append(missing, "from_currency")
	}
	if strings.TrimSpace(req.ToCurrency) == "" {
		missing = append(missing, "to_currency")
	}
	if strings.TrimSpace(req.AddressTo) == "" {
		missing = append(missing, "address_to")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required create params: %s", strings.Join(missing, ", "))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
