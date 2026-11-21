// Package fakeapi is an in-process stand-in for boxbox3's HTTP API, for the
// provider's tests. It follows the contract (docs/api.md in the boxbox3
// repository): statuses, error codes, phases, quota, the asynchronous
// creation and destruction. It is not a recording of the real server; where
// the contract leaves something open, the choice made here is listed in
// docs/terraform.md.
//
// Time does not pass here: a machine moves from `creating` to `ready` (or
// `failed`) and from `destroying` to gone after a set number of `GET`s of
// that machine, so tests are deterministic.
package fakeapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/triac-hq/terraform-provider-boxbox/internal/client"
)

// Node is a node of the fake cluster and the memory it can promise.
type Node struct {
	Name      string
	MemoryMiB int64
}

// Access is the Cloudflare Access service token the fake edge demands.
type Access struct {
	ID     string
	Secret string
}

// Options of a fake.
type Options struct {
	// Hostname is `web.hostname`: a write with an `Origin` must name it.
	Hostname string
	// RetryAfter is the `Retry-After` of `creating` and `destroying` answers.
	// The contract says 5; tests that do not want to wait set 0.
	RetryAfter int
	// CreatePolls is how many `GET`s of a machine answer `creating` before it
	// is `ready`. DestroyPolls likewise for `destroying` before `404`.
	CreatePolls  int
	DestroyPolls int
	Images       []string
	Nodes        []Node
	// AdminImplemented makes the section "defined, not implemented yet"
	// answer in its specified shape instead of `501`.
	AdminImplemented bool
	// Access, when set, puts a Cloudflare Access look-alike in front.
	Access *Access
}

// Request is one request the fake saw.
type Request struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

type machine struct {
	m           client.Machine
	polls       int
	fail        string
	hold        bool
	runCPUMax   int64
	runMemory   client.Size
	restart     *bool
	wantedNodes []string
}

type tenant struct {
	quota   *client.Quota
	email   *string
	network string
	index   int
}

type injected struct {
	status     int
	code       string
	retryAfter string
}

// Server is the fake. It is an http.Handler.
type Server struct {
	mu        sync.Mutex
	opts      Options
	tokens    map[string]string
	admins    map[string]bool
	tenants   map[string]*tenant
	machines  map[string]*machine
	passwords map[string]string
	failNext  map[string]string
	holdNext  map[string]bool
	inject    []injected
	requests  []Request
	nextVMID  int64
	nextAddr  int
	nextNet   int
}

// New makes a fake with the contract's defaults.
func New(opts Options) *Server {
	if opts.Images == nil {
		opts.Images = []string{"debian-13", "ubuntu-24.04"}
	}
	if opts.Nodes == nil {
		opts.Nodes = []Node{{"node1", 64 * 1024}, {"node2", 64 * 1024}, {"node3", 64 * 1024}}
	}
	return &Server{
		opts:      opts,
		tokens:    map[string]string{},
		admins:    map[string]bool{},
		tenants:   map[string]*tenant{},
		machines:  map[string]*machine{},
		passwords: map[string]string{},
		failNext:  map[string]string{},
		holdNext:  map[string]bool{},
		nextVMID:  100,
		nextAddr:  10,
		nextNet:   1,
	}
}

func key(tenant, name string) string { return tenant + "/" + name }

// AddToken makes PVE know a token: `token` is the whole
// `<user>@pve!<tokenid>=<secret>`. allocate is whether the token has
// `VM.Allocate` on `/` (what makes `admin@pve` the owner).
func (s *Server) AddToken(token string, allocate bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = token[:strings.Index(token, "!")]
	s.admins[token] = allocate
}

// AddTenant makes a tenant as the owner's `boxbox3 apply` would.
func (s *Server) AddTenant(name string, quota *client.Quota) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addTenant(name, quota, nil)
}

func (s *Server) addTenant(name string, quota *client.Quota, email *string) *tenant {
	t := &tenant{quota: quota, email: email, network: fmt.Sprintf("10.100.%d.0/24", s.nextNet), index: s.nextNet}
	s.nextNet++
	s.tenants[name] = t
	return t
}

// AddConfigMachine puts a machine of the owner's config into a tenant's pool.
func (s *Server) AddConfigMachine(tenantName, name string, cpu client.CPU, memory, disk client.Size) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mc := s.reserve(tenantName, name, s.opts.Nodes[0].Name)
	mc.m.Source = client.SourceConfig
	mc.m.Phase = client.PhaseReady
	mc.m.Status = "running"
	mc.m.Image, mc.m.User = nil, nil
	mc.m.CPU, mc.m.Memory, mc.m.Disk = cpu, memory, disk
	mc.runCPUMax, mc.runMemory = cpu.Max, memory
}

// FailCreation makes the next creation of the machine end in `failed`.
func (s *Server) FailCreation(tenantName, name, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[key(tenantName, name)] = reason
}

// HoldCreation makes the next creation of the machine stay in `creating`
// until Release.
func (s *Server) HoldCreation(tenantName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdNext[key(tenantName, name)] = true
}

// Release lets a held machine go on.
func (s *Server) Release(tenantName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.holdNext, key(tenantName, name))
	if mc := s.machines[key(tenantName, name)]; mc != nil {
		mc.hold = false
	}
}

// Remove takes a machine away behind the API's back (drift).
func (s *Server) Remove(tenantName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.machines, key(tenantName, name))
}

// StopStart is the tenant stopping and starting the machine from PVE:
// what was pending takes effect.
func (s *Server) StopStart(tenantName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mc := s.machines[key(tenantName, name)]; mc != nil {
		mc.runCPUMax, mc.runMemory = mc.m.CPU.Max, mc.m.Memory
		mc.m.Status = "running"
	}
}

// Stop is the tenant stopping the machine from PVE.
func (s *Server) Stop(tenantName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mc := s.machines[key(tenantName, name)]; mc != nil {
		mc.m.Status = "stopped"
	}
}

// Machine is the machine as `GET` would answer it now, without counting as a
// poll; nil when there is none.
func (s *Server) Machine(tenantName, name string) *client.Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	mc := s.machines[key(tenantName, name)]
	if mc == nil {
		return nil
	}
	m := mc.view()
	return &m
}

// Password is the last password set for the machine's login user.
func (s *Server) Password(tenantName, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.passwords[key(tenantName, name)]
}

// Inject makes the next request (that passed the edge) fail with the status
// and code; retryAfter "" leaves the header out.
func (s *Server) Inject(status int, code, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inject = append(s.inject, injected{status, code, retryAfter})
}

// Requests are the requests seen so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// ResetRequests forgets the requests seen so far.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

func (mc *machine) view() client.Machine {
	m := mc.m
	m.Pending = nil
	if m.Status == "running" && m.Phase != client.PhaseCreating {
		var p client.Pending
		if mc.runCPUMax != m.CPU.Max {
			p.CPUMax = &client.PendingInt{Running: mc.runCPUMax, Configured: m.CPU.Max}
		}
		if mc.runMemory != m.Memory {
			p.Memory = &client.PendingSize{Running: mc.runMemory, Configured: m.Memory}
		}
		if p.CPUMax != nil || p.Memory != nil {
			m.Pending = &p
		}
	}
	if m.HA != nil {
		ha := *m.HA
		ha.Nodes = append([]string{}, ha.Nodes...)
		ha.Restart = mc.restart
		m.HA = &ha
	}
	return m
}

type apiError struct {
	status   int
	code     string
	message  string
	problems []string
	header   map[string]string
}

func errf(status int, code, format string, a ...any) *apiError {
	return &apiError{status: status, code: code, message: fmt.Sprintf(format, a...)}
}

func (s *Server) writeError(w http.ResponseWriter, e *apiError) {
	for k, v := range e.header {
		w.Header().Set(k, v)
	}
	problems := e.problems
	if problems == nil {
		problems = []string{}
	}
	s.writeJSON(w, e.status, map[string]any{"error": map[string]any{
		"code": e.code, "message": e.message, "problems": problems,
	}})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeMachine(w http.ResponseWriter, status int, mc *machine) {
	m := mc.view()
	if m.Phase == client.PhaseCreating || m.Phase == client.PhaseDestroying {
		w.Header().Set("Retry-After", strconv.Itoa(s.opts.RetryAfter))
	}
	s.writeJSON(w, status, m)
}

type caller struct {
	user   string
	admin  bool
	tenant string
}

const prefix = "/boxbox/api/v1/"

// ServeHTTP answers one request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(), Body: body})

	if a := s.opts.Access; a != nil {
		if r.Header.Get("CF-Access-Client-Id") != a.ID || r.Header.Get("CF-Access-Client-Secret") != a.Secret {
			w.Header().Set("Location", "https://fake.cloudflareaccess.com/cdn-cgi/access/login/"+r.Host+"?kid=abc&redirect_url=%2F")
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, "<html>login</html>")
			return
		}
	}
	if len(s.inject) > 0 {
		in := s.inject[0]
		s.inject = s.inject[1:]
		e := errf(in.status, in.code, "injected")
		if in.retryAfter != "" {
			e.header = map[string]string{"Retry-After": in.retryAfter}
		}
		s.writeError(w, e)
		return
	}
	if e := s.route(w, r, body); e != nil {
		s.writeError(w, e)
	}
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, body []byte) *apiError {
	if r.Method == http.MethodOptions {
		return errf(403, "forbidden", "no CORS")
	}
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return errf(404, "not_found", "no such path")
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	type handler func(c caller) *apiError
	var methods map[string]handler
	read := func(h handler) map[string]handler {
		return map[string]handler{http.MethodGet: h, http.MethodHead: h}
	}
	switch {
	case len(parts) == 1 && parts[0] == "me":
		methods = read(func(c caller) *apiError { return s.getMe(w, c) })
	case len(parts) == 1 && parts[0] == "machines":
		methods = read(func(c caller) *apiError { return s.listMachines(w, r, c) })
	case len(parts) == 2 && parts[0] == "machines":
		return errf(404, "not_found", "no such path")
	case len(parts) == 3 && parts[0] == "machines":
		t, n := parts[1], parts[2]
		methods = read(func(c caller) *apiError { return s.getMachine(w, c, t, n) })
		methods[http.MethodPut] = func(c caller) *apiError { return s.putMachine(w, c, t, n, body) }
		methods[http.MethodDelete] = func(c caller) *apiError { return s.deleteMachine(w, c, t, n) }
	case len(parts) == 4 && parts[0] == "machines" && parts[3] == "password":
		t, n := parts[1], parts[2]
		methods = map[string]handler{http.MethodPost: func(c caller) *apiError { return s.setPassword(w, c, t, n, body) }}
	case len(parts) == 4 && parts[0] == "machines" && parts[3] == "adopt":
		t, n := parts[1], parts[2]
		methods = map[string]handler{http.MethodPost: func(c caller) *apiError { return s.adoptMachine(w, c, t, n) }}
	case len(parts) == 1 && parts[0] == "tenants":
		methods = read(func(c caller) *apiError { return s.listTenants(w, c) })
	case len(parts) == 2 && parts[0] == "tenants":
		n := parts[1]
		methods = read(func(c caller) *apiError { return s.getTenant(w, c, n) })
		methods[http.MethodPut] = func(c caller) *apiError { return s.putTenant(w, c, n, body) }
		methods[http.MethodDelete] = func(c caller) *apiError { return s.deleteTenant(w, c, n) }
	default:
		return errf(404, "not_found", "no such path")
	}
	h, ok := methods[r.Method]
	if !ok {
		return errf(405, "method_not_allowed", "%s does not belong to this path", r.Method)
	}
	c, e := s.identify(r)
	if e != nil {
		return e
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if e := s.checkWrite(r, body); e != nil {
			return e
		}
	}
	return h(c)
}

func (s *Server) identify(r *http.Request) (caller, *apiError) {
	auths := r.Header.Values("Authorization")
	cookies := 0
	for _, c := range r.Cookies() {
		if c.Name == "PVEAuthCookie" {
			cookies++
		}
	}
	hasToken := len(auths) > 0 && strings.HasPrefix(auths[0], "PVEAPIToken")
	if hasToken && cookies > 0 {
		return caller{}, errf(400, "ambiguous_credentials", "both a token and a cookie")
	}
	// The fake knows tokens only: a ticket is never valid here.
	if len(auths) != 1 || cookies > 0 || !hasToken {
		return caller{}, errf(401, "unauthenticated", "no credentials PVE accepts")
	}
	token := strings.TrimPrefix(auths[0], "PVEAPIToken=")
	user, ok := s.tokens[token]
	if !ok {
		return caller{}, errf(401, "unauthenticated", "no credentials PVE accepts")
	}
	name, realm, _ := strings.Cut(user, "@")
	if realm != "pve" {
		return caller{}, errf(403, "forbidden", "neither a tenant nor the owner")
	}
	if name == "admin" {
		if !s.admins[token] {
			return caller{}, errf(403, "forbidden", "the token has no VM.Allocate on /")
		}
		return caller{user: user, admin: true}, nil
	}
	if _, ok := s.tenants[name]; !ok {
		return caller{}, errf(403, "forbidden", "neither a tenant nor the owner")
	}
	return caller{user: user, tenant: name}, nil
}

func (s *Server) checkWrite(r *http.Request, body []byte) *apiError {
	if v := r.Header.Get("Sec-Fetch-Site"); v != "" && v != "same-origin" && v != "none" {
		return errf(403, "csrf", "Sec-Fetch-Site")
	}
	if v := r.Header.Get("Origin"); v != "" && v != "https://"+s.opts.Hostname {
		return errf(403, "csrf", "Origin")
	}
	// `Content-Type` is required of a body; DELETE and adopt have none.
	if len(body) == 0 {
		return nil
	}
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		return errf(415, "unsupported_media_type", "the body must be application/json")
	}
	if len(body) > 65536 {
		return errf(413, "too_large", "the body is more than 65536 bytes")
	}
	return nil
}

func decode(body []byte, v any) *apiError {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errf(400, "bad_request", "%v", err)
	}
	if dec.More() {
		return errf(400, "bad_request", "more than one JSON value")
	}
	return nil
}

func (s *Server) usage(tenantName string) client.Usage {
	var u client.Usage
	var mem, disk int64
	for _, mc := range s.machines {
		if mc.m.Tenant != tenantName {
			continue
		}
		u.Machines++
		u.CPU.Min = round05(u.CPU.Min + mc.m.CPU.Min)
		u.CPU.Max += mc.m.CPU.Max
		mem += mc.m.Memory.MiB()
		disk += mc.m.Disk.MiB()
	}
	u.Memory, u.Disk = client.SizeFromMiB(mem), client.SizeFromMiB(disk)
	return u
}

func round05(v float64) float64 { return math.Round(v*20) / 20 }

func (s *Server) getMe(w http.ResponseWriter, c caller) *apiError {
	me := client.Me{User: c.user, Role: client.RoleAdmin, Images: s.opts.Images}
	if !c.admin {
		me.Role = client.RoleTenant
		me.Tenant = &client.MeTenant{Name: c.tenant, Quota: s.tenants[c.tenant].quota, Usage: s.usage(c.tenant)}
	}
	s.writeJSON(w, 200, me)
	return nil
}

func (s *Server) listMachines(w http.ResponseWriter, r *http.Request, c caller) *apiError {
	want := r.URL.Query().Get("tenant")
	if !c.admin {
		if want != "" && want != c.tenant {
			return errf(403, "forbidden", "another tenant")
		}
		want = c.tenant
	}
	out := []client.Machine{}
	for _, mc := range s.machines {
		if want == "" || mc.m.Tenant == want {
			out = append(out, mc.view())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Name < out[j].Name
	})
	s.writeJSON(w, 200, map[string]any{"machines": out})
	return nil
}

// find checks the names and the caller's right to the path.
func (s *Server) find(c caller, tenantName, name string) (*machine, *apiError) {
	if !client.ValidTenantName(tenantName) || !client.ValidMachineName(name) {
		return nil, errf(404, "not_found", "no such path")
	}
	if !c.admin && c.tenant != tenantName {
		return nil, errf(403, "forbidden", "another tenant's path")
	}
	if _, ok := s.tenants[tenantName]; !ok {
		return nil, errf(404, "not_found", "no such tenant")
	}
	return s.machines[key(tenantName, name)], nil
}

func (s *Server) getMachine(w http.ResponseWriter, c caller, tenantName, name string) *apiError {
	mc, e := s.find(c, tenantName, name)
	if e != nil {
		return e
	}
	if mc == nil {
		return errf(404, "not_found", "no such machine")
	}
	// A poll is what moves the background work forward.
	switch mc.m.Phase {
	case client.PhaseCreating:
		if mc.hold {
			break
		}
		if mc.polls > 0 {
			mc.polls--
			break
		}
		s.finish(mc)
	case client.PhaseDestroying:
		if mc.polls > 0 {
			mc.polls--
			break
		}
		delete(s.machines, key(tenantName, name))
		delete(s.passwords, key(tenantName, name))
		return errf(404, "not_found", "no such machine")
	}
	s.writeMachine(w, 200, mc)
	return nil
}

func (s *Server) finish(mc *machine) {
	if mc.fail != "" {
		reason := mc.fail
		mc.m.Phase, mc.m.Reason = client.PhaseFailed, &reason
		return
	}
	mc.m.Phase, mc.m.Reason = client.PhaseReady, nil
	if mc.m.Image != nil {
		mc.m.Status = "running"
	}
	mc.runCPUMax, mc.runMemory = mc.m.CPU.Max, mc.m.Memory
	if mc.m.HA != nil {
		mc.m.HA.Nodes = s.replicaNodes(mc)
		mc.m.HA.Protected = mc.restart == nil || *mc.restart
	}
}

func (s *Server) replicaNodes(mc *machine) []string {
	if mc.wantedNodes != nil {
		return append([]string{}, mc.wantedNodes...)
	}
	out := []string{}
	for _, n := range s.opts.Nodes {
		if n.Name != mc.m.Node && int64(len(out)) < mc.m.HA.Replicas {
			out = append(out, n.Name)
		}
	}
	return out
}

func (s *Server) reserve(tenantName, name, node string) *machine {
	t := s.tenants[tenantName]
	nat := fmt.Sprintf("172.31.0.%d", s.nextAddr)
	internal := fmt.Sprintf("10.100.%d.%d", t.index, s.nextAddr)
	s.nextAddr++
	mc := &machine{m: client.Machine{
		Tenant: tenantName, Name: name, VMID: s.nextVMID, Source: client.SourceAPI,
		Phase: client.PhaseCreating, Node: node, Status: "stopped",
		Addresses: client.Addresses{NAT: &nat, Internal: &internal},
	}}
	s.nextVMID++
	s.machines[key(tenantName, name)] = mc
	return mc
}

type specIn struct {
	Image *string `json:"image"`
	CPU   *struct {
		Min json.RawMessage `json:"min"`
		Max *int64          `json:"max"`
	} `json:"cpu"`
	Memory *string `json:"memory"`
	Disk   *string `json:"disk"`
	HA     *struct {
		Replicas *int64    `json:"replicas"`
		Nodes    *[]string `json:"nodes"`
		Restart  *bool     `json:"restart"`
	} `json:"ha"`
	SSHKeys          *[]string `json:"ssh_keys"`
	TailscaleAuthKey *string   `json:"tailscale_auth_key"`
	Node             *string   `json:"node"`
	User             *string   `json:"user"`
	CloudInit        *string   `json:"cloud_init"`
	ISO              *string   `json:"iso"`
}

func (in *specIn) adminFields() bool {
	return in.Node != nil || in.User != nil || in.CloudInit != nil || in.ISO != nil ||
		(in.HA != nil && (in.HA.Nodes != nil || in.HA.Restart != nil))
}

type spec struct {
	image    *string
	cpu      client.CPU
	memory   client.Size
	disk     client.Size
	replicas int64
	nodes    []string
	restart  *bool
	node     string
	user     string
	iso      bool
	hasKeys  bool
}

var (
	sshKeyRe    = regexp.MustCompile(`^[a-z0-9@.-]+ [A-Za-z0-9+/]+=* ?[^\n]*$`)
	tailscaleRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)
	userRe      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

func (s *Server) validate(in *specIn) (spec, []string) {
	var p []string
	out := spec{cpu: client.CPU{Min: 0, Max: 2}, memory: client.SizeFromMiB(2048), disk: client.SizeFromMiB(10240)}
	if in.ISO != nil {
		out.iso = true
		for name, set := range map[string]bool{"image": in.Image != nil, "ssh_keys": in.SSHKeys != nil, "user": in.User != nil, "cloud_init": in.CloudInit != nil} {
			if set {
				p = append(p, name+": not with iso")
			}
		}
	} else if in.Image == nil {
		p = append(p, "image: required")
	} else {
		found := false
		for _, i := range s.opts.Images {
			found = found || i == *in.Image
		}
		if !found {
			p = append(p, fmt.Sprintf("image: %q is not one of the images", *in.Image))
		}
		out.image = in.Image
	}
	if in.CPU != nil {
		if in.CPU.Max != nil {
			out.cpu.Max = *in.CPU.Max
		}
		if in.CPU.Min != nil {
			raw := strings.Trim(string(in.CPU.Min), `"`)
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				p = append(p, "cpu.min: not a number")
			}
			out.cpu.Min = v
		}
	}
	if out.cpu.Max < 1 || out.cpu.Max > 512 {
		p = append(p, "cpu.max: must be 1-512")
	}
	if out.cpu.Min < 0 || out.cpu.Min > float64(out.cpu.Max) || out.cpu.Min > 100 {
		p = append(p, "cpu.min: must be between 0 and cpu.max, at most 100")
	} else if math.Abs(out.cpu.Min*20-math.Round(out.cpu.Min*20)) > 1e-9 {
		p = append(p, "cpu.min: must be in steps of 0.05")
	}
	if in.Memory != nil {
		v, err := client.ParseSize(*in.Memory)
		if err != nil {
			p = append(p, "memory: "+err.Error())
		}
		out.memory = v
	}
	if in.Disk != nil {
		v, err := client.ParseSize(*in.Disk)
		switch {
		case err != nil:
			p = append(p, "disk: "+err.Error())
		case !v.WholeGiB() || v.MiB() < 4096:
			p = append(p, "disk: must be whole gibibytes, at least 4GiB")
		}
		out.disk = v
	}
	if in.HA != nil {
		out.restart = in.HA.Restart
		switch {
		case in.HA.Nodes != nil:
			out.nodes = *in.HA.Nodes
			out.replicas = int64(len(out.nodes))
			if in.HA.Replicas != nil {
				p = append(p, "ha.nodes: not with ha.replicas")
			}
			if in.Node == nil {
				p = append(p, "ha.nodes: needs node")
			}
		case in.HA.Replicas == nil:
			p = append(p, "ha.replicas: required")
		default:
			out.replicas = *in.HA.Replicas
		}
		if in.HA.Nodes != nil || in.HA.Replicas != nil {
			if out.replicas < 1 || out.replicas > int64(len(s.opts.Nodes)-1) {
				p = append(p, fmt.Sprintf("ha.replicas: must be 1-%d", len(s.opts.Nodes)-1))
			}
		}
	}
	if in.SSHKeys != nil {
		out.hasKeys = true
		if n := len(*in.SSHKeys); n < 1 || n > 16 {
			p = append(p, "ssh_keys: must be 1-16 keys")
		}
		for i, k := range *in.SSHKeys {
			if len(k) > 2048 || !sshKeyRe.MatchString(k) {
				p = append(p, fmt.Sprintf("ssh_keys[%d]: not an OpenSSH public key", i))
			}
		}
	}
	if in.TailscaleAuthKey != nil && !tailscaleRe.MatchString(*in.TailscaleAuthKey) {
		p = append(p, "tailscale_auth_key: must be 1-200 characters of A-Z a-z 0-9 - _")
	}
	if in.Node != nil {
		out.node = *in.Node
		if !s.hasNode(out.node) {
			p = append(p, fmt.Sprintf("node: no node %q", out.node))
		}
	}
	for _, n := range out.nodes {
		if !s.hasNode(n) || n == out.node {
			p = append(p, fmt.Sprintf("ha.nodes: %q is not another node", n))
		}
	}
	if in.User != nil {
		out.user = *in.User
		if !userRe.MatchString(out.user) || out.user == "root" {
			p = append(p, "user: not a login name")
		}
	}
	if in.CloudInit != nil && len(*in.CloudInit) > 32768 {
		p = append(p, "cloud_init: more than 32768 bytes")
	}
	return out, p
}

func (s *Server) hasNode(name string) bool {
	for _, n := range s.opts.Nodes {
		if n.Name == name {
			return true
		}
	}
	return false
}

// free is what each node can still promise, without the machine `skip`.
func (s *Server) free(skip *machine) map[string]int64 {
	free := map[string]int64{}
	for _, n := range s.opts.Nodes {
		free[n.Name] = n.MemoryMiB
	}
	for _, mc := range s.machines {
		if mc == skip {
			continue
		}
		free[mc.m.Node] -= mc.m.Memory.MiB()
		if mc.m.HA != nil {
			for _, n := range s.replicaNodes(mc) {
				free[n] -= mc.m.Memory.MiB()
			}
		}
	}
	return free
}

// place picks the node with the most room, and checks the replicas fit on
// others. An empty answer is `no_room`.
func (s *Server) place(sp spec, existing *machine) string {
	free := s.free(existing)
	need := sp.memory.MiB()
	fits := func(primary string) bool {
		if free[primary] < need {
			return false
		}
		if sp.nodes != nil {
			for _, n := range sp.nodes {
				if free[n] < need {
					return false
				}
			}
			return true
		}
		others := int64(0)
		for _, n := range s.opts.Nodes {
			if n.Name != primary && free[n.Name] >= need {
				others++
			}
		}
		return others >= sp.replicas
	}
	if existing != nil {
		if fits(existing.m.Node) {
			return existing.m.Node
		}
		return ""
	}
	if sp.node != "" {
		if fits(sp.node) {
			return sp.node
		}
		return ""
	}
	best := ""
	for _, n := range s.opts.Nodes {
		if fits(n.Name) && (best == "" || free[n.Name] > free[best]) {
			best = n.Name
		}
	}
	return best
}

func (s *Server) overQuota(tenantName string, q *client.Quota, sp spec, existing *machine) []string {
	u := s.usage(tenantName)
	if existing != nil {
		u.Machines--
		u.CPU.Min = round05(u.CPU.Min - existing.m.CPU.Min)
		u.CPU.Max -= existing.m.CPU.Max
		u.Memory = client.SizeFromMiB(u.Memory.MiB() - existing.m.Memory.MiB())
		u.Disk = client.SizeFromMiB(u.Disk.MiB() - existing.m.Disk.MiB())
	}
	var p []string
	if n := u.Machines + 1; n > q.Machines {
		p = append(p, fmt.Sprintf("%s: %d machines, the quota is %d", tenantName, n, q.Machines))
	}
	if v := round05(u.CPU.Min + sp.cpu.Min); v > q.CPU.Min {
		p = append(p, fmt.Sprintf("%s: cpu.min %v, the quota is %v", tenantName, v, q.CPU.Min))
	}
	if v := u.CPU.Max + sp.cpu.Max; v > q.CPU.Max {
		p = append(p, fmt.Sprintf("%s: cpu.max %d, the quota is %d", tenantName, v, q.CPU.Max))
	}
	if v := u.Memory.MiB() + sp.memory.MiB(); v > q.Memory.MiB() {
		p = append(p, fmt.Sprintf("%s: memory %s, the quota is %s", tenantName, client.SizeFromMiB(v), q.Memory))
	}
	if v := u.Disk.MiB() + sp.disk.MiB(); v > q.Disk.MiB() {
		p = append(p, fmt.Sprintf("%s: disk %s, the quota is %s", tenantName, client.SizeFromMiB(v), q.Disk))
	}
	return p
}

func (s *Server) putMachine(w http.ResponseWriter, c caller, tenantName, name string, body []byte) *apiError {
	mc, e := s.find(c, tenantName, name)
	if e != nil {
		return e
	}
	var in specIn
	if e := decode(body, &in); e != nil {
		return e
	}
	if in.adminFields() {
		if !c.admin {
			return errf(403, "forbidden", "a field of the owner in a tenant's request")
		}
		if !s.opts.AdminImplemented {
			return errf(501, "not_implemented", "the owner's machine fields are not implemented yet")
		}
	}
	sp, problems := s.validate(&in)
	if mc == nil && !sp.iso && !sp.hasKeys {
		problems = append(problems, "ssh_keys: required when the machine is created")
	}
	if problems != nil {
		return &apiError{status: 422, code: "invalid", message: "the values do not do", problems: problems}
	}
	quota := s.tenants[tenantName].quota

	if mc != nil {
		switch {
		case mc.m.Source == client.SourceConfig:
			return errf(409, "not_api_machine", "the machine is made by the owner's config")
		case mc.m.Phase == client.PhaseDestroying:
			return errf(409, "destroying", "the machine is being destroyed")
		}
		was := int64(0)
		if mc.m.HA != nil {
			was = mc.m.HA.Replicas
		}
		changed := mc.m.CPU != sp.cpu || mc.m.Memory != sp.memory || mc.m.Disk != sp.disk || was != sp.replicas
		switch {
		case mc.m.Image != nil && sp.image != nil && *mc.m.Image != *sp.image:
			return errf(409, "image_differs", "the machine was made from %s", *mc.m.Image)
		case sp.disk.MiB() < mc.m.Disk.MiB():
			return errf(409, "cannot_shrink", "the disk is %s", mc.m.Disk)
		case sp.replicas < was:
			return errf(409, "cannot_reduce_ha", "the machine has %d replicas", was)
		case mc.m.Phase == client.PhaseCreating && changed:
			return errf(409, "busy", "the machine is being created")
		}
		if changed {
			if e := s.allowed(c, tenantName, quota, sp, mc); e != nil {
				return e
			}
			if s.place(sp, mc) == "" {
				return &apiError{status: 409, code: "no_room", message: "the machine does not fit", problems: []string{name + ": no node has room"}}
			}
			mc.m.CPU, mc.m.Memory, mc.m.Disk = sp.cpu, sp.memory, sp.disk
			if sp.replicas > 0 {
				if mc.m.HA == nil {
					mc.m.HA = &client.HA{}
				}
				mc.m.HA.Replicas = sp.replicas
				if mc.m.Phase == client.PhaseReady {
					mc.m.HA.Nodes = s.replicaNodes(mc)
					mc.m.HA.Protected = true
				}
			}
		}
		if mc.m.Phase == client.PhaseFailed {
			mc.m.Phase, mc.m.Reason, mc.fail = client.PhaseCreating, nil, ""
			mc.polls = s.opts.CreatePolls
		}
		s.writeMachine(w, 200, mc)
		return nil
	}

	if e := s.allowed(c, tenantName, quota, sp, nil); e != nil {
		return e
	}
	node := s.place(sp, nil)
	if node == "" {
		return &apiError{status: 409, code: "no_room", message: "the machine does not fit", problems: []string{name + ": no node has room"}}
	}
	mc = s.reserve(tenantName, name, node)
	mc.m.Image, mc.m.CPU, mc.m.Memory, mc.m.Disk = sp.image, sp.cpu, sp.memory, sp.disk
	if !sp.iso {
		user := tenantName
		if sp.user != "" {
			user = sp.user
		}
		mc.m.User = &user
	}
	if sp.replicas > 0 {
		mc.m.HA = &client.HA{Replicas: sp.replicas, Nodes: []string{}}
		mc.wantedNodes = sp.nodes
		if s.opts.AdminImplemented {
			restart := sp.restart == nil || *sp.restart
			mc.restart = &restart
		}
	}
	k := key(tenantName, name)
	mc.polls, mc.fail, mc.hold = s.opts.CreatePolls, s.failNext[k], s.holdNext[k]
	delete(s.failNext, k)
	delete(s.holdNext, k)
	s.writeMachine(w, 201, mc)
	return nil
}

// allowed is the quota: a tenant needs one, and nobody may go over it.
func (s *Server) allowed(c caller, tenantName string, quota *client.Quota, sp spec, existing *machine) *apiError {
	if quota == nil {
		if c.admin {
			return nil
		}
		return errf(403, "no_quota", "the tenant has no quota")
	}
	var problems []string
	if sp.replicas > 0 && !quota.HA && !c.admin {
		problems = append(problems, tenantName+": the quota does not allow ha.replicas")
	}
	problems = append(problems, s.overQuota(tenantName, quota, sp, existing)...)
	if problems != nil {
		return &apiError{status: 403, code: "over_quota", message: "the machines are more than the quota of their tenant lets them be", problems: problems}
	}
	return nil
}

func (s *Server) deleteMachine(w http.ResponseWriter, c caller, tenantName, name string) *apiError {
	mc, e := s.find(c, tenantName, name)
	if e != nil {
		return e
	}
	switch {
	case mc == nil:
		return errf(404, "not_found", "no such machine")
	case mc.m.Source == client.SourceConfig:
		return errf(409, "not_api_machine", "the machine is made by the owner's config")
	case mc.m.Phase != client.PhaseDestroying:
		mc.m.Phase, mc.m.Reason, mc.hold = client.PhaseDestroying, nil, false
		mc.polls = s.opts.DestroyPolls
	}
	s.writeMachine(w, 202, mc)
	return nil
}

func (s *Server) adoptMachine(w http.ResponseWriter, c caller, tenantName, name string) *apiError {
	if !c.admin {
		return errf(403, "forbidden", "only the owner adopts a machine")
	}
	mc, e := s.find(c, tenantName, name)
	if e != nil {
		return e
	}
	if mc == nil {
		return errf(404, "not_found", "no such machine")
	}
	mc.m.Source = client.SourceAPI
	s.writeMachine(w, 200, mc)
	return nil
}

func (s *Server) setPassword(w http.ResponseWriter, c caller, tenantName, name string, body []byte) *apiError {
	mc, e := s.find(c, tenantName, name)
	if e != nil {
		return e
	}
	var in struct {
		Password *string `json:"password"`
	}
	if e := decode(body, &in); e != nil {
		return e
	}
	ok := in.Password != nil && len(*in.Password) >= 8 && len(*in.Password) <= 128
	if ok {
		for _, r := range *in.Password {
			ok = ok && r >= 0x20 && r <= 0x7e
		}
	}
	switch {
	case !ok:
		return &apiError{status: 422, code: "invalid", message: "the values do not do", problems: []string{"password: must be 8-128 printable ASCII characters"}}
	case mc == nil:
		return errf(404, "not_found", "no such machine")
	case mc.m.Source == client.SourceConfig:
		return errf(409, "not_api_machine", "the machine is made by the owner's config")
	case mc.m.Phase != client.PhaseReady || mc.m.Status != "running":
		return errf(409, "not_ready", "the machine is not ready and running")
	}
	s.passwords[key(tenantName, name)] = *in.Password
	w.WriteHeader(204)
	return nil
}

func (s *Server) tenantsGate(c caller) *apiError {
	if !c.admin {
		return errf(403, "forbidden", "tenants are the owner's")
	}
	if !s.opts.AdminImplemented {
		return errf(501, "not_implemented", "tenants are not implemented yet")
	}
	return nil
}

func (s *Server) tenantView(name string) client.Tenant {
	t := s.tenants[name]
	return client.Tenant{Name: name, Email: t.email, Quota: t.quota, Usage: s.usage(name), Network: t.network}
}

func (s *Server) listTenants(w http.ResponseWriter, c caller) *apiError {
	if e := s.tenantsGate(c); e != nil {
		return e
	}
	names := make([]string, 0, len(s.tenants))
	for n := range s.tenants {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []client.Tenant{}
	for _, n := range names {
		out = append(out, s.tenantView(n))
	}
	s.writeJSON(w, 200, map[string]any{"tenants": out})
	return nil
}

func (s *Server) getTenant(w http.ResponseWriter, c caller, name string) *apiError {
	if e := s.tenantsGate(c); e != nil {
		return e
	}
	if _, ok := s.tenants[name]; !ok || !client.ValidTenantName(name) {
		return errf(404, "not_found", "no such tenant")
	}
	s.writeJSON(w, 200, s.tenantView(name))
	return nil
}

func (s *Server) putTenant(w http.ResponseWriter, c caller, name string, body []byte) *apiError {
	if e := s.tenantsGate(c); e != nil {
		return e
	}
	if !client.ValidTenantName(name) {
		return errf(404, "not_found", "no such path")
	}
	var in struct {
		Quota *json.RawMessage `json:"quota"`
		Email *string          `json:"email"`
	}
	if e := decode(body, &in); e != nil {
		return e
	}
	// `"quota": null` and a missing key both decode to nil; the contract makes
	// the key required, so look at the body itself.
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(body, &keys)
	if _, ok := keys["quota"]; !ok {
		return &apiError{status: 422, code: "invalid", message: "the values do not do", problems: []string{"quota: required (null takes it away)"}}
	}
	var quota *client.Quota
	if in.Quota != nil {
		var q struct {
			Machines *int64 `json:"machines"`
			CPU      *struct {
				Min *float64 `json:"min"`
				Max *int64   `json:"max"`
			} `json:"cpu"`
			Memory *client.Size `json:"memory"`
			Disk   *client.Size `json:"disk"`
			HA     bool         `json:"ha"`
		}
		if e := decode(*in.Quota, &q); e != nil {
			return e
		}
		if q.Machines == nil || q.CPU == nil || q.CPU.Min == nil || q.CPU.Max == nil || q.Memory == nil || q.Disk == nil {
			return &apiError{status: 422, code: "invalid", message: "the values do not do", problems: []string{"quota: every field but ha is required"}}
		}
		quota = &client.Quota{Machines: *q.Machines, CPU: client.CPU{Min: *q.CPU.Min, Max: *q.CPU.Max}, Memory: *q.Memory, Disk: *q.Disk, HA: q.HA}
	}
	status := 200
	t, ok := s.tenants[name]
	if !ok {
		t = s.addTenant(name, nil, nil)
		status = 201
	}
	t.quota, t.email = quota, in.Email
	s.writeJSON(w, status, s.tenantView(name))
	return nil
}

func (s *Server) deleteTenant(w http.ResponseWriter, c caller, name string) *apiError {
	if e := s.tenantsGate(c); e != nil {
		return e
	}
	if _, ok := s.tenants[name]; !ok || !client.ValidTenantName(name) {
		return errf(404, "not_found", "no such tenant")
	}
	if s.usage(name).Machines > 0 {
		return errf(409, "tenant_has_machines", "the tenant has machines")
	}
	delete(s.tenants, name)
	w.WriteHeader(204)
	return nil
}
