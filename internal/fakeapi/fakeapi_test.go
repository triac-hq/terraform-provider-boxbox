package fakeapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/triac-hq/terraform-provider-boxbox/internal/client"
	"github.com/triac-hq/terraform-provider-boxbox/internal/fakeapi"
)

// These tests pin the fake to the contract: each row is a sentence of
// docs/api.md. They talk raw HTTP, not through the provider's client.

const (
	demo  = "demo@pve!tf=01234567-89ab-cdef-0123-456789abcdef"
	other = "other@pve!tf=11111111-89ab-cdef-0123-456789abcdef"
	admin = "admin@pve!tf=22222222-89ab-cdef-0123-456789abcdef"
	weak  = "admin@pve!weak=33333333-89ab-cdef-0123-456789abcdef"
	root  = "root@pam!x=44444444-89ab-cdef-0123-456789abcdef"
	body  = `{"image":"ubuntu-24.04","ssh_keys":["ssh-ed25519 AAAA demo"]}`
)

type call struct {
	method string
	path   string
	body   string
	header map[string]string
	token  string
}

type env struct {
	t    *testing.T
	fake *fakeapi.Server
	srv  *httptest.Server
}

func newEnv(t *testing.T, opts fakeapi.Options) *env {
	opts.Hostname = "cluster.example.com"
	fake := fakeapi.New(opts)
	gib := func(n int64) client.Size { return client.SizeFromMiB(n * 1024) }
	fake.AddTenant("demo", &client.Quota{Machines: 2, CPU: client.CPU{Min: 2, Max: 8}, Memory: gib(8), Disk: gib(80)})
	fake.AddTenant("other", nil)
	for tok, alloc := range map[string]bool{demo: false, other: false, admin: true, weak: false, root: true} {
		fake.AddToken(tok, alloc)
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return &env{t, fake, srv}
}

// do returns the status, the error code (or "") and the decoded body.
func (e *env) do(c call) (int, string, map[string]any, http.Header) {
	e.t.Helper()
	var rd io.Reader
	if c.body != "" {
		rd = strings.NewReader(c.body)
	}
	req, err := http.NewRequest(c.method, e.srv.URL+"/boxbox/api/v1/"+c.path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if c.token == "" {
		c.token = demo
	}
	if c.token != "none" {
		req.Header.Set("Authorization", "PVEAPIToken="+c.token)
	}
	if c.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.header {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	code := ""
	if len(raw) > 0 {
		if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
			e.t.Errorf("%s %s: content type %q", c.method, c.path, ct)
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			e.t.Fatalf("%s %s: not JSON: %s", c.method, c.path, raw)
		}
		if eo, ok := out["error"].(map[string]any); ok {
			code, _ = eo["code"].(string)
			if _, ok := eo["problems"].([]any); !ok {
				e.t.Errorf("%s %s: problems is not always a list: %s", c.method, c.path, raw)
			}
		}
	}
	return resp.StatusCode, code, out, resp.Header
}

func (e *env) want(name string, c call, status int, code string) map[string]any {
	e.t.Helper()
	gs, gc, out, _ := e.do(c)
	if gs != status || gc != code {
		e.t.Errorf("%s: %s %s = %d %q, want %d %q", name, c.method, c.path, gs, gc, status, code)
	}
	return out
}

func (e *env) ready(name string) {
	e.t.Helper()
	e.want("create "+name, call{method: "PUT", path: "machines/demo/" + name, body: body}, 201, "")
	for i := 0; i < 5; i++ {
		if _, _, out, _ := e.do(call{method: "GET", path: "machines/demo/" + name}); out["phase"] == "ready" {
			return
		}
	}
	e.t.Fatalf("%s did not become ready", name)
}

func TestBasicsAndAuthentication(t *testing.T) {
	e := newEnv(t, fakeapi.Options{})
	m := "machines/demo/web1"
	e.want("no credentials", call{method: "GET", path: "me", token: "none"}, 401, "unauthenticated")
	e.want("unknown token", call{method: "GET", path: "me", token: "demo@pve!tf=nope"}, 401, "unauthenticated")
	e.want("both", call{method: "GET", path: "me", header: map[string]string{"Cookie": "PVEAuthCookie=PVE:demo@pve:abc"}}, 400, "ambiguous_credentials")
	e.want("cookie alone (the fake has no tickets)", call{method: "GET", path: "me", token: "none", header: map[string]string{"Cookie": "PVEAuthCookie=x"}}, 401, "unauthenticated")
	e.want("root@pam", call{method: "GET", path: "me", token: root}, 403, "forbidden")
	e.want("admin token without VM.Allocate", call{method: "GET", path: "me", token: weak}, 403, "forbidden")
	e.want("options", call{method: "OPTIONS", path: "me"}, 403, "forbidden")
	e.want("method", call{method: "PATCH", path: m, body: body}, 405, "method_not_allowed")
	e.want("post to a machine", call{method: "POST", path: m, body: body}, 405, "method_not_allowed")
	e.want("delete me", call{method: "DELETE", path: "me"}, 405, "method_not_allowed")
	e.want("no such path", call{method: "GET", path: "nothing"}, 404, "not_found")
	e.want("bad name in the path", call{method: "GET", path: "machines/demo/Web_1"}, 404, "not_found")
	e.want("another tenant's path", call{method: "GET", path: "machines/other/web1"}, 403, "forbidden")
	e.want("another tenant's list", call{method: "GET", path: "machines?tenant=other"}, 403, "forbidden")
	e.want("content type", call{method: "PUT", path: m, body: body, header: map[string]string{"Content-Type": "text/plain"}}, 415, "unsupported_media_type")
	e.want("too large", call{method: "PUT", path: m, body: `{"image":"` + strings.Repeat("a", 70000) + `"}`}, 413, "too_large")
	e.want("not JSON", call{method: "PUT", path: m, body: `{`}, 400, "bad_request")
	e.want("unknown field", call{method: "PUT", path: m, body: `{"image":"ubuntu-24.04","colour":"red"}`}, 400, "bad_request")
	e.want("wrong type", call{method: "PUT", path: m, body: `{"image":"ubuntu-24.04","cpu":{"max":"two"}}`}, 400, "bad_request")
	e.want("foreign origin", call{method: "PUT", path: m, body: body, header: map[string]string{"Origin": "https://evil.example"}}, 403, "csrf")
	e.want("cross-site fetch", call{method: "PUT", path: m, body: body, header: map[string]string{"Sec-Fetch-Site": "cross-site"}}, 403, "csrf")
	e.want("head", call{method: "HEAD", path: "me"}, 200, "")

	out := e.want("me", call{method: "GET", path: "me"}, 200, "")
	if out["user"] != "demo@pve" || out["role"] != "tenant" {
		t.Errorf("me: %v", out)
	}
	out = e.want("me as the owner", call{method: "GET", path: "me", token: admin}, 200, "")
	if out["role"] != "admin" || out["tenant"] != nil {
		t.Errorf("me as the owner: %v", out)
	}
	out = e.want("empty list", call{method: "GET", path: "machines"}, 200, "")
	if l, ok := out["machines"].([]any); !ok || len(l) != 0 {
		t.Errorf("an empty list must be []: %v", out)
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t, fakeapi.Options{})
	out := e.want("everything wrong", call{method: "PUT", path: "machines/demo/web1", body: `{
		"image":"windows","cpu":{"min":0.07,"max":0},"memory":"0GiB","disk":"3GiB","ha":{},
		"ssh_keys":[],"tailscale_auth_key":"has space"}`}, 422, "invalid")
	problems := out["error"].(map[string]any)["problems"].([]any)
	if len(problems) < 7 {
		t.Errorf("422 must list every finding, got %v", problems)
	}
	e.want("no keys at creation", call{method: "PUT", path: "machines/demo/web1", body: `{"image":"ubuntu-24.04"}`}, 422, "invalid")
	e.want("no image", call{method: "PUT", path: "machines/demo/web1", body: `{"ssh_keys":["ssh-ed25519 AAAA x"]}`}, 422, "invalid")
	e.want("cpu.min above cpu.max", call{method: "PUT", path: "machines/demo/web1", body: `{"image":"ubuntu-24.04","cpu":{"min":3,"max":2},"ssh_keys":["ssh-ed25519 AAAA x"]}`}, 422, "invalid")
	out = e.want("cpu.min as a string", call{method: "PUT", path: "machines/demo/web1", body: `{"image":"ubuntu-24.04","cpu":{"min":"0.5"},"memory":"3072MiB","ssh_keys":["ssh-ed25519 AAAA x"]}`}, 201, "")
	if out["cpu"].(map[string]any)["min"] != 0.5 || out["cpu"].(map[string]any)["max"] != float64(2) || out["memory"] != "3GiB" || out["disk"] != "10GiB" {
		t.Errorf("defaults and canonical forms: %v", out)
	}
	if out["phase"] != "creating" || out["source"] != "api" || out["ha"] != nil || out["pending"] != nil || out["reason"] != nil {
		t.Errorf("a new machine: %v", out)
	}
}

func TestMachineStates(t *testing.T) {
	e := newEnv(t, fakeapi.Options{CreatePolls: 1, DestroyPolls: 1, RetryAfter: 5})
	m := "machines/demo/web1"
	put := func(b string) call { return call{method: "PUT", path: m, body: b} }
	with := func(extra string) string {
		return `{"image":"ubuntu-24.04","ssh_keys":["ssh-ed25519 AAAA demo"]` + extra + `}`
	}

	_, _, _, h := e.do(put(body))
	if h.Get("Retry-After") != "5" {
		t.Errorf("201 without Retry-After: 5")
	}
	e.want("same body while creating", put(body), 200, "")
	e.want("create-only fields may be left out", put(`{"image":"ubuntu-24.04"}`), 200, "")
	e.want("change while creating", put(with(`,"memory":"4GiB"`)), 409, "busy")
	e.want("password before ready", call{method: "POST", path: m + "/password", body: `{"password":"correct horse"}`}, 409, "not_ready")
	_, _, out, h := e.do(call{method: "GET", path: m})
	if out["phase"] != "creating" || h.Get("Retry-After") != "5" {
		t.Errorf("first poll: %v %v", out["phase"], h.Get("Retry-After"))
	}
	_, _, out, h = e.do(call{method: "GET", path: m})
	if out["phase"] != "ready" || out["status"] != "running" || h.Get("Retry-After") != "" {
		t.Errorf("second poll: %v", out)
	}

	e.want("image", put(`{"image":"debian-13"}`), 409, "image_differs")
	e.want("shrink", put(with(`,"disk":"8GiB"`)), 409, "cannot_shrink")
	e.want("ha without quota.ha", put(with(`,"ha":{"replicas":1}`)), 403, "over_quota")
	e.want("over quota", put(with(`,"memory":"9GiB"`)), 403, "over_quota")
	e.want("quota may fill exactly", put(with(`,"memory":"8GiB","cpu":{"min":2,"max":8}`)), 200, "")
	out = e.want("back down", put(with(`,"memory":"4GiB","disk":"20GiB"`)), 200, "")
	p := out["pending"].(map[string]any)
	if p["cpu_max"] != nil || p["memory"].(map[string]any)["running"] != "2GiB" || p["memory"].(map[string]any)["configured"] != "4GiB" || out["disk"] != "20GiB" {
		t.Errorf("pending after a change of a running machine: %v", out)
	}
	e.fake.StopStart("demo", "web1")
	if _, _, out, _ = e.do(call{method: "GET", path: m}); out["pending"] != nil {
		t.Errorf("pending after stop and start: %v", out["pending"])
	}
	e.want("bad password", call{method: "POST", path: m + "/password", body: `{"password":"short"}`}, 422, "invalid")
	e.want("password", call{method: "POST", path: m + "/password", body: `{"password":"correct horse"}`}, 204, "")
	e.fake.Stop("demo", "web1")
	e.want("password of a stopped machine", call{method: "POST", path: m + "/password", body: `{"password":"correct horse"}`}, 409, "not_ready")

	out = e.want("delete", call{method: "DELETE", path: m}, 202, "")
	if out["phase"] != "destroying" {
		t.Errorf("delete: %v", out)
	}
	e.want("delete again", call{method: "DELETE", path: m}, 202, "")
	e.want("put while destroying", put(body), 409, "destroying")
	e.want("still there", call{method: "GET", path: m}, 200, "")
	e.want("gone", call{method: "GET", path: m}, 404, "not_found")
	e.want("delete of nothing", call{method: "DELETE", path: m}, 404, "not_found")
}

func TestQuotaRoomAndConfigMachines(t *testing.T) {
	e := newEnv(t, fakeapi.Options{Nodes: []fakeapi.Node{{Name: "n1", MemoryMiB: 4096}, {Name: "n2", MemoryMiB: 3072}}})
	e.want("no quota", call{method: "PUT", path: "machines/other/web1", body: body, token: other}, 403, "no_quota")
	e.want("the owner is not bound by a missing quota", call{method: "PUT", path: "machines/other/web1", body: body, token: admin}, 201, "")
	e.want("the owner is bound by a quota", call{method: "PUT", path: "machines/demo/big", token: admin,
		body: `{"image":"ubuntu-24.04","memory":"9GiB","ssh_keys":["ssh-ed25519 AAAA x"]}`}, 403, "over_quota")
	e.want("no room", call{method: "PUT", path: "machines/demo/big",
		body: `{"image":"ubuntu-24.04","memory":"4GiB","ssh_keys":["ssh-ed25519 AAAA x"]}`}, 409, "no_room")
	out := e.want("fits on the other node", call{method: "PUT", path: "machines/demo/web1",
		body: `{"image":"ubuntu-24.04","memory":"3GiB","ssh_keys":["ssh-ed25519 AAAA x"]}`}, 201, "")
	if out["node"] != "n2" {
		t.Errorf("placed on %v", out["node"])
	}
	e.want("third machine", call{method: "PUT", path: "machines/demo/web2",
		body: `{"image":"ubuntu-24.04","memory":"3GiB","ssh_keys":["ssh-ed25519 AAAA x"]}`}, 409, "no_room")

	e.fake.AddConfigMachine("demo", "hub", client.CPU{Min: 1, Max: 2}, client.SizeFromMiB(1024), client.SizeFromMiB(10240))
	out = e.want("config machine reads", call{method: "GET", path: "machines/demo/hub"}, 200, "")
	if out["source"] != "config" || out["image"] != nil || out["user"] != nil {
		t.Errorf("config machine: %v", out)
	}
	e.want("config machine put", call{method: "PUT", path: "machines/demo/hub", body: body}, 409, "not_api_machine")
	e.want("config machine delete", call{method: "DELETE", path: "machines/demo/hub"}, 409, "not_api_machine")
	e.want("config machine password", call{method: "POST", path: "machines/demo/hub/password", body: `{"password":"correct horse"}`}, 409, "not_api_machine")
	e.want("adopt as a tenant", call{method: "POST", path: "machines/demo/hub/adopt"}, 403, "forbidden")
	e.want("adopt with GET", call{method: "GET", path: "machines/demo/hub/adopt", token: admin}, 405, "method_not_allowed")
	e.want("adopt of nothing", call{method: "POST", path: "machines/demo/nothing/adopt", token: admin}, 404, "not_found")
	e.want("a config machine uses quota", call{method: "PUT", path: "machines/demo/web3", body: body}, 403, "over_quota")
	out = e.want("the owner sees all, by tenant then name", call{method: "GET", path: "machines", token: admin}, 200, "")
	var names []string
	for _, m := range out["machines"].([]any) {
		names = append(names, m.(map[string]any)["tenant"].(string)+"/"+m.(map[string]any)["name"].(string))
	}
	if strings.Join(names, " ") != "demo/hub demo/web1 other/web1" {
		t.Errorf("list order: %v", names)
	}
	for i := 0; i < 2; i++ {
		if out = e.want("adopt", call{method: "POST", path: "machines/demo/hub/adopt", token: admin}, 200, ""); out["source"] != "api" {
			t.Errorf("adopted: %v", out)
		}
	}
	e.want("an adopted machine is the API's", call{method: "DELETE", path: "machines/demo/hub"}, 202, "")
}

func TestNotImplementedSection(t *testing.T) {
	e := newEnv(t, fakeapi.Options{})
	e.want("tenants as a tenant", call{method: "GET", path: "tenants"}, 403, "forbidden")
	e.want("tenants", call{method: "GET", path: "tenants", token: admin}, 501, "not_implemented")
	e.want("tenant", call{method: "PUT", path: "tenants/uusi", token: admin, body: `{"quota":null}`}, 501, "not_implemented")
	e.want("the owner's field as a tenant", call{method: "PUT", path: "machines/demo/web1",
		body: `{"image":"ubuntu-24.04","node":"node1","ssh_keys":["ssh-ed25519 AAAA x"]}`}, 403, "forbidden")
	e.want("the owner's field as the owner", call{method: "PUT", path: "machines/demo/web1", token: admin,
		body: `{"image":"ubuntu-24.04","ha":{"replicas":1,"restart":false},"ssh_keys":["ssh-ed25519 AAAA x"]}`}, 501, "not_implemented")

	e = newEnv(t, fakeapi.Options{AdminImplemented: true})
	e.want("quota key is required", call{method: "PUT", path: "tenants/uusi", token: admin, body: `{"email":"a@example.com"}`}, 422, "invalid")
	e.want("quota fields are required", call{method: "PUT", path: "tenants/uusi", token: admin, body: `{"quota":{"machines":1}}`}, 422, "invalid")
	out := e.want("new tenant", call{method: "PUT", path: "tenants/uusi", token: admin,
		body: `{"quota":{"machines":1,"cpu":{"min":1,"max":2},"memory":"2048MiB","disk":"10GiB"}}`}, 201, "")
	q := out["quota"].(map[string]any)
	if q["ha"] != false || q["memory"] != "2GiB" || out["email"] != nil || out["network"] == "" {
		t.Errorf("tenant: %v", out)
	}
	e.want("existing tenant", call{method: "PUT", path: "tenants/uusi", token: admin, body: `{"quota":null}`}, 200, "")
	out = e.want("named nodes", call{method: "PUT", path: "machines/demo/web1", token: admin,
		body: `{"image":"ubuntu-24.04","node":"node2","user":"ops","ha":{"nodes":["node3"],"restart":false},"ssh_keys":["ssh-ed25519 AAAA x"]}`}, 201, "")
	if out["node"] != "node2" || out["user"] != "ops" || out["ha"].(map[string]any)["restart"] != false {
		t.Errorf("the owner's fields: %v", out)
	}
	e.want("delete a tenant with machines", call{method: "DELETE", path: "tenants/demo", token: admin}, 409, "tenant_has_machines")
	e.want("delete", call{method: "DELETE", path: "tenants/uusi", token: admin}, 204, "")
	e.want("delete of nothing", call{method: "DELETE", path: "tenants/uusi", token: admin}, 404, "not_found")
}
