package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	apiPath = "/boxbox/api/v1/"
	// The contract's poll interval when an answer does not carry `Retry-After`.
	defaultPoll = 5 * time.Second
	maxWait     = 60 * time.Second
	maxBody     = 4 << 20
)

var tokenRe = regexp.MustCompile(`^([a-z][a-z0-9]{1,7})@pve![A-Za-z][A-Za-z0-9._-]*=(\S+)$`)

// Config is what a Client needs.
type Config struct {
	// Endpoint is `https://<web.hostname>`, without the API path.
	Endpoint string
	// APIToken is the PVE API token: `<user>@pve!<tokenid>=<secret>`.
	APIToken string
	// AccessClientID and AccessClientSecret are the Cloudflare Access service
	// token. Both or neither.
	AccessClientID     string
	AccessClientSecret string
	UserAgent          string
	HTTPClient         *http.Client
	// Attempts is how many times a request is sent when the answer is 502,
	// 503 or 504 or the connection fails. Every write is idempotent.
	Attempts int
	// Backoff is the first wait between attempts; it doubles.
	Backoff time.Duration
	// MinPoll is the shortest wait between polls, whatever `Retry-After` says.
	MinPoll time.Duration
}

// Client talks to one boxbox3 API.
type Client struct {
	base    *url.URL
	cfg     Config
	http    *http.Client
	user    string
	secrets []string
}

// APIError is an answer with status 4xx or 5xx.
type APIError struct {
	Method   string
	Path     string
	Status   int
	Code     string
	Message  string
	Problems []string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	for _, p := range e.Problems {
		b.WriteString("\n  - " + p)
	}
	return b.String()
}

// IsCode tells whether err is an APIError with the code.
func IsCode(err error, code string) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == code
}

// IsNotFound tells whether err is the API's `404 not_found`.
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// IsNotImplemented tells whether the server answered `501`.
func IsNotImplemented(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotImplemented
}

// WaitError is a wait that ran out of time. Last is the last answer seen.
type WaitError struct {
	Last *Machine
	Err  error
}

func (e *WaitError) Error() string {
	phase := "unknown"
	if e.Last != nil {
		phase = e.Last.Phase
	}
	return fmt.Sprintf("gave up waiting (last phase %q): %v", phase, e.Err)
}

func (e *WaitError) Unwrap() error { return e.Err }

// New checks the configuration and makes a Client. Its errors never contain
// a secret.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("endpoint is empty")
	}
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || u.Host == "" {
		return nil, errors.New("endpoint is not a URL like https://cluster.example.com")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("endpoint must be only scheme and host, like https://cluster.example.com (the API path is added)")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return nil, errors.New("endpoint must be https (http is accepted only for a loopback address)")
		}
	default:
		return nil, errors.New("endpoint must be https")
	}
	m := tokenRe.FindStringSubmatch(cfg.APIToken)
	if m == nil {
		return nil, errors.New("api_token is not a PVE API token of the form <user>@pve!<tokenid>=<secret>")
	}
	if (cfg.AccessClientID == "") != (cfg.AccessClientSecret == "") {
		return nil, errors.New("access_client_id and access_client_secret go together: set both or neither")
	}
	if strings.ContainsAny(cfg.AccessClientID+cfg.AccessClientSecret, "\r\n") {
		return nil, errors.New("the Cloudflare Access service token has a line break in it")
	}
	if cfg.Attempts < 1 {
		cfg.Attempts = 4
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = time.Second
	}
	if cfg.MinPoll <= 0 {
		cfg.MinPoll = time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "terraform-provider-boxbox"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 90 * time.Second}
	}
	cp := *hc
	// A redirect is never part of the API: it is Cloudflare Access sending an
	// unauthenticated caller to its login page.
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	u.Path = ""
	c := &Client{base: u, cfg: cfg, http: &cp, user: m[1] + "@pve"}
	for _, s := range []string{cfg.APIToken, m[2], cfg.AccessClientSecret, cfg.AccessClientID} {
		if s != "" {
			c.secrets = append(c.secrets, s)
		}
	}
	return c, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// User is the PVE user of the token (`demo@pve`), read from the token itself.
func (c *Client) User() string { return c.user }

// TokenTenant is the tenant the token belongs to, or "" for the owner's token.
func (c *Client) TokenTenant() string {
	name := strings.TrimSuffix(c.user, "@pve")
	if name == "admin" {
		return ""
	}
	return name
}

// Secrets are the values that must never be logged.
func (c *Client) Secrets() []string { return append([]string(nil), c.secrets...) }

// Redact takes every secret of the client out of a text.
func (c *Client) Redact(s string) string {
	for _, sec := range c.secrets {
		s = strings.ReplaceAll(s, sec, "***")
	}
	return s
}

type result struct {
	status     int
	retryAfter time.Duration
	hasRetry   bool
}

// do sends one request, again when the failure is one the contract lets a
// client repeat, and decodes the answer into out when it is 2xx with a body.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) (result, error) {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return result{}, fmt.Errorf("%s %s: encoding the request: %w", method, path, err)
		}
	}
	wait := c.cfg.Backoff
	for attempt := 1; ; attempt++ {
		res, err, again := c.once(ctx, method, path, query, body, out)
		if err == nil || !again || attempt >= c.cfg.Attempts {
			return res, err
		}
		pause := wait
		if res.hasRetry {
			pause = res.retryAfter
		}
		wait *= 2
		if serr := sleep(ctx, min(pause, maxWait)); serr != nil {
			return res, fmt.Errorf("%w (while waiting to retry after: %v)", serr, err)
		}
	}
}

func (c *Client) once(ctx context.Context, method, path string, query url.Values, body []byte, out any) (result, error, bool) {
	u := *c.base
	u.Path = apiPath + path
	u.RawQuery = query.Encode()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return result{}, fmt.Errorf("%s %s: %s", method, path, c.Redact(err.Error())), false
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.cfg.APIToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.AccessClientID != "" {
		req.Header.Set("CF-Access-Client-Id", c.cfg.AccessClientID)
		req.Header.Set("CF-Access-Client-Secret", c.cfg.AccessClientSecret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return result{}, ctx.Err(), false
		}
		return result{}, fmt.Errorf("%s %s: %s", method, path, c.Redact(err.Error())), true
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		if ctx.Err() != nil {
			return result{}, ctx.Err(), false
		}
		return result{}, fmt.Errorf("%s %s: reading the answer: %s", method, path, c.Redact(err.Error())), true
	}
	res := result{status: resp.StatusCode}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, perr := strconv.Atoi(s); perr == nil && n >= 0 {
			res.retryAfter, res.hasRetry = time.Duration(n)*time.Second, true
		}
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	isJSON := mt == "application/json"

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if resp.StatusCode == http.StatusNoContent || out == nil {
			return res, nil, false
		}
		if !isJSON {
			return res, c.notAPI(method, path, resp), false
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return res, fmt.Errorf("%s %s: HTTP %d with a body that is not the contract's JSON: %s", method, path, resp.StatusCode, c.Redact(err.Error())), false
		}
		return res, nil, false
	}
	if resp.StatusCode < 400 || !isJSON {
		return res, c.notAPI(method, path, resp), false
	}
	var env struct {
		Error struct {
			Code     string   `json:"code"`
			Message  string   `json:"message"`
			Problems []string `json:"problems"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Error.Code == "" {
		return res, c.notAPI(method, path, resp), false
	}
	ae := &APIError{Method: method, Path: path, Status: resp.StatusCode, Code: env.Error.Code, Message: c.Redact(env.Error.Message)}
	for _, p := range env.Error.Problems {
		ae.Problems = append(ae.Problems, c.Redact(p))
	}
	again := resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout
	return res, ae, again
}

// notAPI describes an answer that did not come from the API: every answer of
// the API is JSON, so this is the edge (Cloudflare Access) or a wrong address.
func (c *Client) notAPI(method, path string, resp *http.Response) error {
	what := fmt.Sprintf("HTTP %d that is not from the boxbox3 API (content type %q)", resp.StatusCode, resp.Header.Get("Content-Type"))
	hint := "check that endpoint is the name of the cluster's web interface"
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		host := ""
		if loc, err := url.Parse(resp.Header.Get("Location")); err == nil {
			host = loc.Host
		}
		what = fmt.Sprintf("HTTP %d, a redirect to %q", resp.StatusCode, host)
		hint = "this is Cloudflare Access asking for a login: the service token (access_client_id, access_client_secret) is missing, wrong, expired or not allowed by the Access policy"
	} else if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		hint = "this is most likely Cloudflare Access refusing the service token (access_client_id, access_client_secret)"
	}
	return fmt.Errorf("%s %s: %s; %s", method, path, what, hint)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func machinePath(tenant, name string) string {
	return "machines/" + url.PathEscape(tenant) + "/" + url.PathEscape(name)
}

// Me is `GET me`.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	if _, err := c.do(ctx, http.MethodGet, "me", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListMachines is `GET machines`; tenant "" asks for all the caller may see.
func (c *Client) ListMachines(ctx context.Context, tenant string) ([]Machine, error) {
	var q url.Values
	if tenant != "" {
		q = url.Values{"tenant": {tenant}}
	}
	var out struct {
		Machines []Machine `json:"machines"`
	}
	if _, err := c.do(ctx, http.MethodGet, "machines", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Machines, nil
}

func (c *Client) getMachine(ctx context.Context, tenant, name string) (*Machine, result, error) {
	var out Machine
	res, err := c.do(ctx, http.MethodGet, machinePath(tenant, name), nil, nil, &out)
	if err != nil {
		return nil, res, err
	}
	return &out, res, nil
}

// GetMachine is `GET machines/{tenant}/{name}`.
func (c *Client) GetMachine(ctx context.Context, tenant, name string) (*Machine, error) {
	m, _, err := c.getMachine(ctx, tenant, name)
	return m, err
}

// PutMachine is `PUT machines/{tenant}/{name}`. created tells whether the
// machine was reserved by this request (`201`).
func (c *Client) PutMachine(ctx context.Context, tenant, name string, spec MachineSpec) (m *Machine, created bool, err error) {
	var out Machine
	res, err := c.do(ctx, http.MethodPut, machinePath(tenant, name), nil, spec, &out)
	if err != nil {
		return nil, false, err
	}
	return &out, res.status == http.StatusCreated, nil
}

// DeleteMachine is `DELETE machines/{tenant}/{name}`. A machine that is
// already gone is the API's `404` (IsNotFound).
func (c *Client) DeleteMachine(ctx context.Context, tenant, name string) (*Machine, error) {
	var out Machine
	if _, err := c.do(ctx, http.MethodDelete, machinePath(tenant, name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AdoptMachine is `POST machines/{tenant}/{name}/adopt` (owner only): a
// machine made by the owner's config becomes an `api` machine. Idempotent.
func (c *Client) AdoptMachine(ctx context.Context, tenant, name string) (*Machine, error) {
	var out Machine
	if _, err := c.do(ctx, http.MethodPost, machinePath(tenant, name)+"/adopt", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetPassword is `POST machines/{tenant}/{name}/password`.
func (c *Client) SetPassword(ctx context.Context, tenant, name, password string) error {
	in := struct {
		Password string `json:"password"`
	}{password}
	_, err := c.do(ctx, http.MethodPost, machinePath(tenant, name)+"/password", nil, in, nil)
	return err
}

func (c *Client) pollWait(res result) time.Duration {
	d := defaultPoll
	if res.hasRetry {
		d = res.retryAfter
	}
	return min(max(d, c.cfg.MinPoll), maxWait)
}

// WaitCreated polls until the machine's phase is `ready` or `failed`, as the
// contract says: `GET`, then wait what `Retry-After` asks. It is bounded by
// ctx; when ctx ends first the error is a *WaitError.
func (c *Client) WaitCreated(ctx context.Context, tenant, name string) (*Machine, error) {
	var last *Machine
	for {
		m, res, err := c.getMachine(ctx, tenant, name)
		if err != nil {
			if ctx.Err() != nil {
				return last, &WaitError{Last: last, Err: ctx.Err()}
			}
			return last, err
		}
		last = m
		if m.Phase == PhaseReady || m.Phase == PhaseFailed {
			return m, nil
		}
		if err := sleep(ctx, c.pollWait(res)); err != nil {
			return last, &WaitError{Last: last, Err: err}
		}
	}
}

// WaitGone polls until `GET` answers `404`.
func (c *Client) WaitGone(ctx context.Context, tenant, name string) error {
	var last *Machine
	for {
		m, res, err := c.getMachine(ctx, tenant, name)
		if IsNotFound(err) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return &WaitError{Last: last, Err: ctx.Err()}
			}
			return err
		}
		last = m
		if err := sleep(ctx, c.pollWait(res)); err != nil {
			return &WaitError{Last: last, Err: err}
		}
	}
}

// ListTenants is `GET tenants` (owner only).
func (c *Client) ListTenants(ctx context.Context) ([]Tenant, error) {
	var out struct {
		Tenants []Tenant `json:"tenants"`
	}
	if _, err := c.do(ctx, http.MethodGet, "tenants", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Tenants, nil
}

// GetTenant is `GET tenants/{name}` (owner only).
func (c *Client) GetTenant(ctx context.Context, name string) (*Tenant, error) {
	var out Tenant
	if _, err := c.do(ctx, http.MethodGet, "tenants/"+url.PathEscape(name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PutTenant is `PUT tenants/{name}` (owner only).
func (c *Client) PutTenant(ctx context.Context, name string, spec TenantSpec) (*Tenant, error) {
	var out Tenant
	if _, err := c.do(ctx, http.MethodPut, "tenants/"+url.PathEscape(name), nil, spec, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteTenant is `DELETE tenants/{name}` (owner only).
func (c *Client) DeleteTenant(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "tenants/"+url.PathEscape(name), nil, nil, nil)
	return err
}
