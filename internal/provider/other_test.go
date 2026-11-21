package provider

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/triac-hq/terraform-provider-boxbox/internal/client"
	"github.com/triac-hq/terraform-provider-boxbox/internal/fakeapi"
)

func cpu(minThreads float64, maxThreads int64) client.CPU {
	return client.CPU{Min: minThreads, Max: maxThreads}
}

// put is somebody else (as demo) changing the cluster past Terraform.
func (e *testEnv) put(name, body string) (int, error) {
	req, err := http.NewRequest(http.MethodPut, e.url+"/boxbox/api/v1/machines/demo/"+name, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+demoToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("PUT %s: %d %s", name, resp.StatusCode, raw)
	}
	return resp.StatusCode, nil
}

func TestAccDataSources(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	e.fake.AddConfigMachine("demo", "hub", cpu(1, 2), gib(2), gib(10))
	cfg := e.provider(demoToken) + machine("  cpu = {\n    min = 0.5\n    max = 4\n  }\n  ha = {\n    replicas = 1\n  }") + `
data "boxbox_me" "me" {
  depends_on = [boxbox_machine.web]
}
data "boxbox_machine" "web" {
  name = boxbox_machine.web.name
}
data "boxbox_machine" "hub" {
  tenant = "demo"
  name   = "hub"
}
data "boxbox_machines" "all" {
  depends_on = [boxbox_machine.web]
}
data "boxbox_machines" "mine" {
  tenant     = "demo"
  depends_on = [boxbox_machine.web]
}
`
	eq := resource.TestCheckResourceAttr
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy:             e.gone("web1"),
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: resource.ComposeAggregateTestCheckFunc(
				eq("data.boxbox_me.me", "user", "demo@pve"),
				eq("data.boxbox_me.me", "role", "tenant"),
				eq("data.boxbox_me.me", "tenant", "demo"),
				eq("data.boxbox_me.me", "quota.machines", "3"),
				eq("data.boxbox_me.me", "quota.cpu.min", "4"),
				eq("data.boxbox_me.me", "quota.cpu.max", "16"),
				eq("data.boxbox_me.me", "quota.memory", "16GiB"),
				eq("data.boxbox_me.me", "quota.disk", "100GiB"),
				eq("data.boxbox_me.me", "quota.ha", "true"),
				eq("data.boxbox_me.me", "usage.machines", "2"),
				eq("data.boxbox_me.me", "usage.cpu.min", "1.5"),
				eq("data.boxbox_me.me", "usage.cpu.max", "6"),
				eq("data.boxbox_me.me", "usage.memory", "4GiB"),
				eq("data.boxbox_me.me", "usage.disk", "20GiB"),
				eq("data.boxbox_me.me", "images.#", "2"),
				eq("data.boxbox_me.me", "images.1", "ubuntu-24.04"),

				eq("data.boxbox_machine.web", "id", "demo/web1"),
				eq("data.boxbox_machine.web", "tenant", "demo"),
				eq("data.boxbox_machine.web", "source", "api"),
				eq("data.boxbox_machine.web", "phase", "ready"),
				eq("data.boxbox_machine.web", "image", "ubuntu-24.04"),
				eq("data.boxbox_machine.web", "cpu.min", "0.5"),
				eq("data.boxbox_machine.web", "ha.replicas", "1"),
				eq("data.boxbox_machine.web", "ha.nodes.#", "1"),
				eq("data.boxbox_machine.web", "ha.protected", "true"),
				resource.TestCheckNoResourceAttr("data.boxbox_machine.web", "ha.restart"),
				resource.TestCheckResourceAttrPair("data.boxbox_machine.web", "vmid", web, "vmid"),
				resource.TestCheckResourceAttrPair("data.boxbox_machine.web", "node", web, "current_node"),
				resource.TestCheckResourceAttrPair("data.boxbox_machine.web", "addresses.nat", web, "addresses.nat"),

				eq("data.boxbox_machine.hub", "source", "config"),
				eq("data.boxbox_machine.hub", "memory", "2GiB"),
				resource.TestCheckNoResourceAttr("data.boxbox_machine.hub", "image"),
				resource.TestCheckNoResourceAttr("data.boxbox_machine.hub", "user"),
				resource.TestCheckNoResourceAttr("data.boxbox_machine.hub", "ha.%"),

				eq("data.boxbox_machines.all", "machines.#", "2"),
				eq("data.boxbox_machines.all", "machines.0.name", "hub"),
				eq("data.boxbox_machines.all", "machines.1.name", "web1"),
				eq("data.boxbox_machines.all", "machines.1.cpu.max", "4"),
				eq("data.boxbox_machines.mine", "machines.#", "2"),
			),
		}, {
			Config:      e.provider(demoToken) + "data \"boxbox_machine\" \"none\" {\n  name = \"nothing\"\n}\n",
			ExpectError: regexp.MustCompile(`not_found`),
		}, {
			Config:      e.provider(demoToken) + "data \"boxbox_machines\" \"other\" {\n  tenant = \"other\"\n}\n",
			ExpectError: regexp.MustCompile(`forbidden`),
		}},
	})
}

func TestAccDataSources_owner(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	e.fake.AddConfigMachine("demo", "hub", cpu(1, 2), gib(2), gib(10))
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: e.provider(adminToken) + "data \"boxbox_me\" \"me\" {}\ndata \"boxbox_machines\" \"all\" {}\n",
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.boxbox_me.me", "role", "admin"),
				resource.TestCheckNoResourceAttr("data.boxbox_me.me", "tenant"),
				resource.TestCheckNoResourceAttr("data.boxbox_me.me", "quota.%"),
				resource.TestCheckNoResourceAttr("data.boxbox_me.me", "usage.%"),
				resource.TestCheckResourceAttr("data.boxbox_machines.all", "machines.#", "1"),
			),
		}, {
			Config:      e.provider(adminToken) + "data \"boxbox_machine\" \"hub\" {\n  name = \"hub\"\n}\n",
			ExpectError: regexp.MustCompile(`Missing\s+tenant`),
		}},
	})
}

const uusi = "boxbox_tenant.uusi"

func tenantConfig(body string) string {
	return "resource \"boxbox_tenant\" \"uusi\" {\n  name = \"uusi\"\n" + body + "\n}\n"
}

// `boxbox_tenant` against the shape the contract specifies.
func TestAccTenant_lifecycle(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{AdminImplemented: true})
	quota := func(machines int, memory string) string {
		return fmt.Sprintf("  quota = {\n    machines = %d\n    cpu = {\n      min = 1.5\n      max = 4\n    }\n    memory = %q\n    disk = \"40GiB\"\n  }", machines, memory)
	}
	update := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(uusi, plancheck.ResourceActionUpdate)}}
	noop := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(uusi, plancheck.ResourceActionNoop)}}
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy: func(*terraform.State) error {
			c, _ := client.New(client.Config{Endpoint: e.url, APIToken: adminToken})
			if _, err := c.GetTenant(t.Context(), "uusi"); !client.IsNotFound(err) {
				return fmt.Errorf("the tenant is still there: %v", err)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: e.provider(adminToken) + tenantConfig("  email = \"uusi@example.com\"\n"+quota(2, "8GiB")),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(uusi, "id", "uusi"),
					resource.TestCheckResourceAttr(uusi, "email", "uusi@example.com"),
					resource.TestCheckResourceAttr(uusi, "quota.machines", "2"),
					resource.TestCheckResourceAttr(uusi, "quota.cpu.min", "1.5"),
					resource.TestCheckResourceAttr(uusi, "quota.memory", "8GiB"),
					resource.TestCheckResourceAttr(uusi, "quota.ha", "false"),
					resource.TestCheckResourceAttr(uusi, "usage.machines", "0"),
					resource.TestCheckResourceAttrSet(uusi, "network"),
				),
			},
			{ResourceName: uusi, ImportState: true, ImportStateId: "uusi", ImportStateVerify: true},
			// The same amount in another unit is not a change.
			{Config: e.provider(adminToken) + tenantConfig("  email = \"uusi@example.com\"\n"+quota(2, "8192MiB")), ConfigPlanChecks: noop},
			{
				Config:           e.provider(adminToken) + tenantConfig(quota(5, "16GiB")),
				ConfigPlanChecks: update,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(uusi, "quota.machines", "5"),
					resource.TestCheckResourceAttr(uusi, "quota.memory", "16GiB"),
					resource.TestCheckNoResourceAttr(uusi, "email"),
				),
			},
			{
				Config:           e.provider(adminToken) + tenantConfig(""),
				ConfigPlanChecks: update,
				Check:            resource.TestCheckNoResourceAttr(uusi, "quota.%"),
			},
			{
				Config:      e.provider(adminToken) + "resource \"boxbox_tenant\" \"demo\" {\n  name = \"demo\"\n}\n",
				ExpectError: regexp.MustCompile(`already\s+exists`),
			},
			{
				Config:      e.provider(adminToken) + "resource \"boxbox_tenant\" \"bad\" {\n  name = \"admin\"\n}\n",
				ExpectError: regexp.MustCompile(`admin`),
			},
		},
	})
}

func TestAccTenant_hasMachines(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{AdminImplemented: true})
	both := e.provider(adminToken) + tenantConfig("") + "resource \"boxbox_machine\" \"web\" {\n  tenant = boxbox_tenant.uusi.name\n  name = \"web1\"\n  image = \"debian-13\"\n  ssh_keys = [\"" + sshKey + "\"]\n}\n"
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: both, Check: resource.TestCheckResourceAttr(web, "id", "uusi/web1")},
			{
				// A machine made past Terraform keeps the tenant from going.
				PreConfig: func() {
					c, _ := client.New(client.Config{Endpoint: e.url, APIToken: adminToken})
					mem, disk := gib(2), gib(10)
					if _, _, err := c.PutMachine(t.Context(), "uusi", "extra", client.MachineSpec{Image: "debian-13", Memory: &mem, Disk: &disk, SSHKeys: []string{sshKey}}); err != nil {
						t.Fatal(err)
					}
				},
				Config:      both,
				Destroy:     true,
				ExpectError: regexp.MustCompile(`tenant_has_machines`),
			},
			{PreConfig: func() { e.fake.Remove("uusi", "extra") }, Config: e.provider(adminToken) + tenantConfig("")},
		},
	})
}

// Today the server answers 501 to tenants: a clear diagnostic, and to a
// tenant's own token the API says forbidden.
func TestAccTenant_notImplemented(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: e.provider(adminToken) + tenantConfig(""), ExpectError: regexp.MustCompile(`the\s+server\s+does\s+not\s+implement\s+this\s+yet`)},
			{Config: e.provider(demoToken) + tenantConfig(""), ExpectError: regexp.MustCompile(`forbidden`)},
		},
	})
}

// The built binary under the real CLI with a dev override, as a user runs
// it: the warning about a pending change reaches the terminal, a second plan
// is empty, and no secret is in the output, the trace log or the state.
func TestCLI_builtProviderWithDevOverride(t *testing.T) {
	tofu := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if tofu == "" {
		t.Skip("no tofu or terraform binary")
	}
	e := newTestEnv(t, fakeapi.Options{Access: &fakeapi.Access{ID: "id.access", Secret: "edge-secret-value"}})
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "terraform-provider-boxbox"), "github.com/triac-hq/terraform-provider-boxbox")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	rc := filepath.Join(dir, "tofurc")
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(rc, fmt.Sprintf("provider_installation {\n  dev_overrides {\n    \"triac-hq/boxbox\" = %q\n  }\n  direct {}\n}\n", bin))
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	config := func(cpuMax int) string {
		return fmt.Sprintf(`terraform {
  required_providers {
    boxbox = { source = "triac-hq/boxbox" }
  }
}
provider "boxbox" {}
resource "boxbox_machine" "web" {
  name               = "web1"
  image              = "ubuntu-24.04"
  cpu                = { max = %d }
  ssh_keys           = [%q]
  tailscale_auth_key = "tskey-auth-kSECRETKEY"
  password           = "console-pass-SECRET"
  password_version   = 1
}
`, cpuMax, sshKey)
	}
	secrets := []string{demoSecret, "edge-secret-value", "tskey-auth-kSECRETKEY", "console-pass-SECRET"}
	run := func(wantExit int, args ...string) string {
		t.Helper()
		cmd := exec.Command(tofu, args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"TF_CLI_CONFIG_FILE="+rc, "TF_LOG=TRACE", "TF_IN_AUTOMATION=1", "CHECKPOINT_DISABLE=1",
			EnvEndpoint+"="+e.url, EnvAPIToken+"="+demoToken,
			EnvAccessClientID+"=id.access", EnvAccessClientSecret+"=edge-secret-value",
			// The provider polls as the server asks; the fake asks for no wait.
		)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		exit := 0
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		text := out.String()
		if exit != wantExit {
			t.Fatalf("%v: exit %d, want %d\n%s", args, exit, wantExit, tail(text))
		}
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("%v: the output (with TF_LOG=TRACE) has the secret %q in it", args, s)
			}
		}
		return text
	}

	write(filepath.Join(work, "main.tf"), config(2))
	run(0, "apply", "-auto-approve", "-no-color")
	if got := e.fake.Password("demo", "web1"); got != "console-pass-SECRET" {
		t.Errorf("the console password is %q", got)
	}
	run(0, "plan", "-no-color", "-detailed-exitcode")

	write(filepath.Join(work, "main.tf"), config(4))
	out := run(0, "apply", "-auto-approve", "-no-color")
	if !regexp.MustCompile(`Warning: The change takes effect when the machine is next stopped and\s+started`).MatchString(out) ||
		!strings.Contains(out, "cpu.max: the machine runs with 2, 4 is configured") {
		t.Errorf("the warning about the pending change is not in the output:\n%s", tail(out))
	}
	// Pending is reported, not a diff: the plan after the apply is empty.
	run(0, "plan", "-no-color", "-detailed-exitcode")

	state, err := os.ReadFile(filepath.Join(work, "terraform.tfstate"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if bytes.Contains(state, []byte(s)) {
			t.Errorf("the state file has the secret %q in it", s)
		}
	}
	if !bytes.Contains(state, []byte(`"configured": 4`)) {
		t.Errorf("the state does not have the pending change")
	}
	run(0, "destroy", "-auto-approve", "-no-color")
	if m := e.fake.Machine("demo", "web1"); m != nil {
		t.Errorf("the machine is still there after destroy: %+v", m)
	}
}

func tail(s string) string {
	// The trace log is long; the CLI's own lines do not start with a date.
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT`).MatchString(l) {
			keep = append(keep, l)
		}
	}
	if len(keep) > 80 {
		keep = keep[len(keep)-80:]
	}
	return strings.Join(keep, "\n")
}
