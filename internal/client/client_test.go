package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/triac-hq/terraform-provider-boxbox/internal/client"
	"github.com/triac-hq/terraform-provider-boxbox/internal/fakeapi"
)

const (
	demoToken  = "demo@pve!tf=01234567-89ab-cdef-0123-456789abcdef"
	otherToken = "other@pve!tf=11111111-89ab-cdef-0123-456789abcdef"
	adminToken = "admin@pve!tf=22222222-89ab-cdef-0123-456789abcdef"
	sshKey     = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIabc demo@laptop"
)

func size(t *testing.T, s string) client.Size {
	t.Helper()
	v, err := client.ParseSize(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func quota(t *testing.T) *client.Quota {
	return &client.Quota{Machines: 2, CPU: client.CPU{Min: 2, Max: 8}, Memory: size(t, "8GiB"), Disk: size(t, "80GiB")}
}

func setup(t *testing.T, opts fakeapi.Options) (*fakeapi.Server, *httptest.Server) {
	t.Helper()
	fake := fakeapi.New(opts)
	fake.AddTenant("demo", quota(t))
	fake.AddTenant("other", nil)
	fake.AddToken(demoToken, false)
	fake.AddToken(otherToken, false)
	fake.AddToken(adminToken, true)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return fake, srv
}

func newClient(t *testing.T, url, token string) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{Endpoint: url, APIToken: token, MinPoll: time.Millisecond, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func basicSpec(t *testing.T) client.MachineSpec {
	mem, disk := size(t, "2GiB"), size(t, "10GiB")
	return client.MachineSpec{Image: "ubuntu-24.04", CPU: &client.CPU{Max: 2}, Memory: &mem, Disk: &disk, SSHKeys: []string{sshKey}}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]string{
		"4GiB": "4GiB", "512MiB": "512MiB", "4096MiB": "4GiB", "1TiB": "1024GiB", "1025MiB": "1025MiB",
	} {
		got, err := client.ParseSize(in)
		if err != nil || got.String() != want {
			t.Errorf("ParseSize(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0GiB", "4 GiB", "4G", "4gib", "-1GiB", "1.5GiB", "99999999999999999999GiB", "9223372036854775807TiB"} {
		if _, err := client.ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) succeeded", in)
		}
	}
	b, _ := json.Marshal(struct{ S client.Size }{size(t, "2048MiB")})
	if string(b) != `{"S":"2GiB"}` {
		t.Errorf("marshal: %s", b)
	}
}

func TestNames(t *testing.T) {
	for _, n := range []string{"demo", "ab", "a1234567"} {
		if !client.ValidTenantName(n) {
			t.Errorf("tenant %q refused", n)
		}
	}
	for _, n := range []string{"a", "admin", "1abc", "abcdefghi", "Demo", "sa-ku"} {
		if client.ValidTenantName(n) {
			t.Errorf("tenant %q accepted", n)
		}
	}
	for _, n := range []string{"a", "web1", "web-1", strings.Repeat("a", 63)} {
		if !client.ValidMachineName(n) {
			t.Errorf("machine %q refused", n)
		}
	}
	for _, n := range []string{"", "-web", "web-", "Web", "we_b", strings.Repeat("a", 64)} {
		if client.ValidMachineName(n) {
			t.Errorf("machine %q accepted", n)
		}
	}
}

func TestNewRefusesBadConfiguration(t *testing.T) {
	secret := "01234567-89ab-cdef-0123-456789abcdef"
	cases := map[string]client.Config{
		"no endpoint":   {APIToken: demoToken},
		"plain http":    {Endpoint: "http://cluster.example.com", APIToken: demoToken},
		"path":          {Endpoint: "https://cluster.example.com/boxbox/api/v1", APIToken: demoToken},
		"userinfo":      {Endpoint: "https://u:p@cluster.example.com", APIToken: demoToken},
		"token form":    {Endpoint: "https://cluster.example.com", APIToken: "demo@pam!tf=" + secret},
		"token prefix":  {Endpoint: "https://cluster.example.com", APIToken: "PVEAPIToken=" + demoToken},
		"half a pair":   {Endpoint: "https://cluster.example.com", APIToken: demoToken, AccessClientID: "id.access"},
		"other half":    {Endpoint: "https://cluster.example.com", APIToken: demoToken, AccessClientSecret: "s3cr3t-value"},
		"token missing": {Endpoint: "https://cluster.example.com"},
	}
	for name, cfg := range cases {
		_, err := client.New(cfg)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "s3cr3t-value") {
			t.Errorf("%s: the error has a secret in it: %v", name, err)
		}
	}
	c, err := client.New(client.Config{Endpoint: "https://cluster.example.com/", APIToken: adminToken})
	if err != nil {
		t.Fatal(err)
	}
	if c.User() != "admin@pve" || c.TokenTenant() != "" {
		t.Errorf("admin: %q %q", c.User(), c.TokenTenant())
	}
}

func TestHeaders(t *testing.T) {
	fake, srv := setup(t, fakeapi.Options{Access: &fakeapi.Access{ID: "id.access", Secret: "s3cr3t-value"}})
	c, err := client.New(client.Config{Endpoint: srv.URL, APIToken: demoToken, AccessClientID: "id.access", AccessClientSecret: "s3cr3t-value"})
	if err != nil {
		t.Fatal(err)
	}
	if c.TokenTenant() != "demo" {
		t.Fatalf("tenant %q", c.TokenTenant())
	}
	if _, _, err := c.PutMachine(context.Background(), "demo", "web1", basicSpec(t)); err != nil {
		t.Fatal(err)
	}
	req := fake.Requests()[0]
	want := map[string]string{
		"Authorization":           "PVEAPIToken=" + demoToken,
		"Cf-Access-Client-Id":     "id.access",
		"Cf-Access-Client-Secret": "s3cr3t-value",
		"Content-Type":            "application/json",
		"Accept":                  "application/json",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"Cookie", "Origin", "X-Boxbox-Csrf"} {
		if req.Header.Get(k) != "" {
			t.Errorf("%s was sent", k)
		}
	}
	if req.Method != "PUT" || req.Path != "/boxbox/api/v1/machines/demo/web1" {
		t.Errorf("request %s %s", req.Method, req.Path)
	}
}

func TestAccessRefusal(t *testing.T) {
	_, srv := setup(t, fakeapi.Options{Access: &fakeapi.Access{ID: "id.access", Secret: "right-secret"}})
	c, err := client.New(client.Config{Endpoint: srv.URL, APIToken: demoToken, AccessClientID: "id.access", AccessClientSecret: "wrong-secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Me(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Cloudflare Access") || !strings.Contains(err.Error(), "fake.cloudflareaccess.com") {
		t.Fatalf("want an error naming Cloudflare Access, got %v", err)
	}
	for _, s := range c.Secrets() {
		if strings.Contains(err.Error(), s) {
			t.Errorf("the error has a secret in it: %v", err)
		}
	}
}

func TestMachineLifecycle(t *testing.T) {
	ctx := context.Background()
	fake, srv := setup(t, fakeapi.Options{CreatePolls: 2, DestroyPolls: 1})
	c := newClient(t, srv.URL, demoToken)

	me, err := c.Me(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if me.User != "demo@pve" || me.Role != client.RoleTenant || me.Tenant.Quota.Machines != 2 || me.Tenant.Usage.Machines != 0 || len(me.Images) != 2 {
		t.Fatalf("me: %+v", me)
	}

	spec := basicSpec(t)
	spec.TailscaleAuthKey = "tskey-auth-abc"
	m, created, err := c.PutMachine(ctx, "demo", "web1", spec)
	if err != nil || !created || m.Phase != client.PhaseCreating || m.VMID == 0 {
		t.Fatalf("put: %+v %v %v", m, created, err)
	}
	if again, created, err := c.PutMachine(ctx, "demo", "web1", spec); err != nil || created || again.VMID != m.VMID {
		t.Fatalf("the same put again: %+v %v %v", again, created, err)
	}
	fake.ResetRequests()
	ready, err := c.WaitCreated(ctx, "demo", "web1")
	if err != nil || ready.Phase != client.PhaseReady || ready.Status != "running" || *ready.User != "demo" {
		t.Fatalf("wait: %+v %v", ready, err)
	}
	if n := len(fake.Requests()); n != 3 {
		t.Errorf("%d polls, want 3 (two creating, one ready)", n)
	}

	list, err := c.ListMachines(ctx, "")
	if err != nil || len(list) != 1 || list[0].Name != "web1" {
		t.Fatalf("list: %+v %v", list, err)
	}

	bigger := size(t, "4GiB")
	spec.Memory = &bigger
	spec.CPU.Max = 4
	changed, _, err := c.PutMachine(ctx, "demo", "web1", spec)
	if err != nil {
		t.Fatal(err)
	}
	if p := changed.Pending; p == nil || p.CPUMax.Running != 2 || p.CPUMax.Configured != 4 || p.Memory.Configured.String() != "4GiB" {
		t.Fatalf("pending: %+v", changed.Pending)
	}

	if err := c.SetPassword(ctx, "demo", "web1", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if fake.Password("demo", "web1") != "correct horse" {
		t.Error("the password did not arrive")
	}

	d, err := c.DeleteMachine(ctx, "demo", "web1")
	if err != nil || d.Phase != client.PhaseDestroying {
		t.Fatalf("delete: %+v %v", d, err)
	}
	if err := c.WaitGone(ctx, "demo", "web1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteMachine(ctx, "demo", "web1"); !client.IsNotFound(err) {
		t.Fatalf("delete of a gone machine: %v", err)
	}
	if _, err := c.GetMachine(ctx, "demo", "web1"); !client.IsNotFound(err) {
		t.Fatalf("get of a gone machine: %v", err)
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	fake, srv := setup(t, fakeapi.Options{})
	demo := newClient(t, srv.URL, demoToken)

	spec := basicSpec(t)
	big := size(t, "16GiB")
	spec.Memory = &big
	_, _, err := demo.PutMachine(ctx, "demo", "web1", spec)
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Status != 403 || ae.Code != "over_quota" || len(ae.Problems) != 1 || !strings.Contains(ae.Problems[0], "memory 16GiB") {
		t.Fatalf("over quota: %v", err)
	}
	if !strings.Contains(err.Error(), "the quota is 8GiB") {
		t.Errorf("the problems are not in the text: %v", err)
	}

	if _, err := demo.GetMachine(ctx, "other", "web1"); !client.IsCode(err, "forbidden") {
		t.Errorf("another tenant's machine: %v", err)
	}
	if _, err := demo.ListMachines(ctx, "other"); !client.IsCode(err, "forbidden") {
		t.Errorf("another tenant's list: %v", err)
	}
	if _, _, err := newClient(t, srv.URL, otherToken).PutMachine(ctx, "other", "web1", basicSpec(t)); !client.IsCode(err, "no_quota") {
		t.Errorf("no quota: %v", err)
	}
	if _, err := newClient(t, srv.URL, "demo@pve!tf=ffffffff-0000-0000-0000-000000000000").Me(ctx); !client.IsCode(err, "unauthenticated") {
		t.Errorf("unknown token: %v", err)
	}
	if _, err := demo.GetTenant(ctx, "demo"); !client.IsCode(err, "forbidden") {
		t.Errorf("tenants as a tenant: %v", err)
	}
	admin := newClient(t, srv.URL, adminToken)
	if _, err := admin.GetTenant(ctx, "demo"); !client.IsNotImplemented(err) {
		t.Errorf("tenants as the owner: %v", err)
	}
	withNode := basicSpec(t)
	withNode.Node = "node2"
	if _, _, err := admin.PutMachine(ctx, "demo", "web1", withNode); !client.IsNotImplemented(err) {
		t.Errorf("the owner's field as the owner: %v", err)
	}
	if _, _, err := demo.PutMachine(ctx, "demo", "web1", withNode); !client.IsCode(err, "forbidden") {
		t.Errorf("the owner's field as a tenant: %v", err)
	}

	fake.FailCreation("demo", "web2", "importing the disk failed")
	if _, _, err := demo.PutMachine(ctx, "demo", "web2", basicSpec(t)); err != nil {
		t.Fatal(err)
	}
	m, err := demo.WaitCreated(ctx, "demo", "web2")
	if err != nil || m.Phase != client.PhaseFailed || *m.Reason != "importing the disk failed" {
		t.Fatalf("failed creation: %+v %v", m, err)
	}
	// The contract: the same PUT again makes the server try again.
	if m, _, err = demo.PutMachine(ctx, "demo", "web2", basicSpec(t)); err != nil || m.Phase != client.PhaseCreating {
		t.Fatalf("retry of a failed machine: %+v %v", m, err)
	}
}

func TestWaitTimesOut(t *testing.T) {
	fake, srv := setup(t, fakeapi.Options{})
	c := newClient(t, srv.URL, demoToken)
	fake.HoldCreation("demo", "web1")
	if _, _, err := c.PutMachine(context.Background(), "demo", "web1", basicSpec(t)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	last, err := c.WaitCreated(ctx, "demo", "web1")
	var we *client.WaitError
	if !errors.As(err, &we) || we.Last == nil || we.Last.Phase != client.PhaseCreating || last == nil {
		t.Fatalf("want a WaitError with the last answer, got %+v %v", last, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the cause is not the deadline: %v", err)
	}
}

func TestPollWaitsWhatRetryAfterAsks(t *testing.T) {
	fake, srv := setup(t, fakeapi.Options{RetryAfter: 1, CreatePolls: 1})
	c := newClient(t, srv.URL, demoToken)
	if _, _, err := c.PutMachine(context.Background(), "demo", "web1", basicSpec(t)); err != nil {
		t.Fatal(err)
	}
	fake.ResetRequests()
	start := time.Now()
	if _, err := c.WaitCreated(context.Background(), "demo", "web1"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < time.Second || d > 3*time.Second {
		t.Errorf("one creating answer with Retry-After: 1 took %s", d)
	}
	if n := len(fake.Requests()); n != 2 {
		t.Errorf("%d polls, want 2", n)
	}
}

func TestRetries(t *testing.T) {
	ctx := context.Background()
	fake, srv := setup(t, fakeapi.Options{})
	c := newClient(t, srv.URL, demoToken)

	fake.Inject(503, "locked", "0")
	fake.Inject(502, "unavailable", "")
	fake.Inject(504, "timeout", "")
	if _, created, err := c.PutMachine(ctx, "demo", "web1", basicSpec(t)); err != nil || !created {
		t.Fatalf("three passing failures: %v", err)
	}
	if n := len(fake.Requests()); n != 4 {
		t.Errorf("%d requests, want 4", n)
	}

	fake.ResetRequests()
	for i := 0; i < 4; i++ {
		fake.Inject(503, "cluster_unavailable", "")
	}
	if _, err := c.Me(ctx); !client.IsCode(err, "cluster_unavailable") {
		t.Fatalf("four failures: %v", err)
	}
	if n := len(fake.Requests()); n != 4 {
		t.Errorf("%d requests, want 4 (bounded)", n)
	}

	fake.ResetRequests()
	fake.Inject(501, "not_implemented", "")
	if _, err := c.Me(ctx); !client.IsNotImplemented(err) {
		t.Fatalf("501: %v", err)
	}
	if n := len(fake.Requests()); n != 1 {
		t.Errorf("501 was retried: %d requests", n)
	}
}

func TestAnswersThatAreNotTheContract(t *testing.T) {
	secret := "01234567-89ab-cdef-0123-456789abcdef"
	mux := http.NewServeMux()
	mux.HandleFunc("/boxbox/api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"user":"demo@pve","role":"tenant","tenant":null,"images":[],"new_field":{"x":1}}`))
	})
	mux.HandleFunc("/boxbox/api/v1/machines/demo/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(422)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "invalid", "message": "got " + r.Header.Get("Authorization"), "problems": []string{"token: " + secret},
		}})
	})
	mux.HandleFunc("/boxbox/api/v1/machines/demo/html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(403)
		_, _ = w.Write([]byte("<html>blocked</html>"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newClient(t, srv.URL, demoToken)

	if me, err := c.Me(context.Background()); err != nil || me.User != "demo@pve" {
		t.Fatalf("an unknown field in an answer must be ignored: %v", err)
	}
	_, err := c.GetMachine(context.Background(), "demo", "echo")
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("a secret echoed by the server must be taken out: %v", err)
	}
	_, err = c.GetMachine(context.Background(), "demo", "html")
	if err == nil || !strings.Contains(err.Error(), "Cloudflare Access") {
		t.Fatalf("an HTML 403 is the edge: %v", err)
	}
}

func TestAdopt(t *testing.T) {
	ctx := context.Background()
	fake, srv := setup(t, fakeapi.Options{})
	fake.AddConfigMachine("demo", "hub", client.CPU{Min: 1, Max: 2}, size(t, "2GiB"), size(t, "10GiB"))
	demo, admin := newClient(t, srv.URL, demoToken), newClient(t, srv.URL, adminToken)
	if _, err := demo.DeleteMachine(ctx, "demo", "hub"); !client.IsCode(err, "not_api_machine") {
		t.Fatalf("delete of a config machine: %v", err)
	}
	if _, err := demo.AdoptMachine(ctx, "demo", "hub"); !client.IsCode(err, "forbidden") {
		t.Fatalf("adopt as a tenant: %v", err)
	}
	if _, err := admin.AdoptMachine(ctx, "demo", "nothing"); !client.IsNotFound(err) {
		t.Fatalf("adopt of nothing: %v", err)
	}
	for i := 0; i < 2; i++ {
		m, err := admin.AdoptMachine(ctx, "demo", "hub")
		if err != nil || m.Source != client.SourceAPI || m.Image != nil {
			t.Fatalf("adopt (time %d): %+v %v", i+1, m, err)
		}
	}
	if m, err := demo.DeleteMachine(ctx, "demo", "hub"); err != nil || m.Phase != client.PhaseDestroying {
		t.Fatalf("delete of an adopted machine: %+v %v", m, err)
	}
}

func TestTenants(t *testing.T) {
	ctx := context.Background()
	_, srv := setup(t, fakeapi.Options{AdminImplemented: true})
	admin := newClient(t, srv.URL, adminToken)

	email := "uusi@example.com"
	got, err := admin.PutTenant(ctx, "uusi", client.TenantSpec{Quota: quota(t), Email: &email})
	if err != nil || got.Name != "uusi" || got.Quota.Memory.String() != "8GiB" || *got.Email != email || got.Network == "" {
		t.Fatalf("put: %+v %v", got, err)
	}
	got, err = admin.PutTenant(ctx, "uusi", client.TenantSpec{})
	if err != nil || got.Quota != nil || got.Email != nil {
		t.Fatalf("null takes the quota and the address away: %+v %v", got, err)
	}
	list, err := admin.ListTenants(ctx)
	if err != nil || len(list) != 3 || list[0].Name != "demo" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if _, _, err := admin.PutMachine(ctx, "uusi", "web1", basicSpec(t)); err != nil {
		t.Fatalf("the owner is not bound by a missing quota: %v", err)
	}
	if err := admin.DeleteTenant(ctx, "uusi"); !client.IsCode(err, "tenant_has_machines") {
		t.Fatalf("delete with machines: %v", err)
	}
	if err := admin.DeleteTenant(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.GetTenant(ctx, "other"); !client.IsNotFound(err) {
		t.Fatalf("get of a deleted tenant: %v", err)
	}
}
