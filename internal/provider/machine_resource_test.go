package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/triac-hq/terraform-provider-boxbox/internal/fakeapi"
)

const web = "boxbox_machine.web"

// machine is a `boxbox_machine.web` named web1 with the given extra lines.
func machine(extra string) string {
	return fmt.Sprintf("resource \"boxbox_machine\" \"web\" {\n  name     = \"web1\"\n  image    = \"ubuntu-24.04\"\n  ssh_keys = [%q]\n%s\n}\n", sshKey, extra)
}

func action(a plancheck.ResourceActionType) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(web, a)}}
}

// lastPut is the body of the last PUT of the machine the fake saw.
func (e *testEnv) lastPut(name string) map[string]any {
	e.t.Helper()
	reqs := e.fake.Requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Method == "PUT" && strings.HasSuffix(reqs[i].Path, "/"+name) {
			var out map[string]any
			if err := json.Unmarshal(reqs[i].Body, &out); err != nil {
				e.t.Fatal(err)
			}
			return out
		}
	}
	e.t.Fatalf("no PUT of %s was seen", name)
	return nil
}

func vmidIs(want *string, same bool) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		got := s.RootModule().Resources[web].Primary.Attributes["vmid"]
		if *want == "" {
			*want = got
			return nil
		}
		if (got == *want) != same {
			return fmt.Errorf("vmid is %s, was %s; want the same machine: %v", got, *want, same)
		}
		*want = got
		return nil
	}
}

// Create, read, a plan without changes, import, update in place, delete.
func TestAccMachine_lifecycle(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	var vmid string
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy:             e.gone("web1"),
		Steps: []resource.TestStep{
			{
				Config: e.provider(demoToken) + machine(`  tailscale_auth_key = "tskey-auth-kSECRETKEY"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(web, "id", "demo/web1"),
					resource.TestCheckResourceAttr(web, "tenant", "demo"),
					resource.TestCheckResourceAttr(web, "phase", "ready"),
					resource.TestCheckResourceAttr(web, "status", "running"),
					resource.TestCheckResourceAttr(web, "source", "api"),
					resource.TestCheckResourceAttr(web, "user", "demo"),
					resource.TestCheckResourceAttr(web, "cpu.min", "0"),
					resource.TestCheckResourceAttr(web, "cpu.max", "2"),
					resource.TestCheckResourceAttr(web, "memory", "2GiB"),
					resource.TestCheckResourceAttr(web, "disk", "10GiB"),
					resource.TestCheckResourceAttr(web, "ha_nodes.#", "0"),
					resource.TestCheckResourceAttr(web, "ha_protected", "false"),
					resource.TestCheckResourceAttrSet(web, "vmid"),
					resource.TestCheckResourceAttrSet(web, "current_node"),
					resource.TestCheckResourceAttrSet(web, "addresses.nat"),
					resource.TestCheckResourceAttrSet(web, "addresses.internal"),
					resource.TestCheckNoResourceAttr(web, "reason"),
					resource.TestCheckNoResourceAttr(web, "pending.%"),
					vmidIs(&vmid, true),
					func(*terraform.State) error {
						put := e.lastPut("web1")
						if put["tailscale_auth_key"] != "tskey-auth-kSECRETKEY" || put["image"] != "ubuntu-24.04" || put["memory"] != "2GiB" {
							return fmt.Errorf("the PUT was %v", put)
						}
						for _, k := range []string{"node", "user", "cloud_init", "iso"} {
							if _, ok := put[k]; ok {
								return fmt.Errorf("a tenant's PUT has the owner's field %s", k)
							}
						}
						return nil
					},
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(web, tfjsonpath.New("tailscale_auth_key"), knownvalue.Null()),
					statecheck.ExpectKnownValue(web, tfjsonpath.New("ha"), knownvalue.Null()),
				},
			},
			{
				ResourceName:      web,
				ImportState:       true,
				ImportStateId:     "demo/web1",
				ImportStateVerify: true,
				// The API never returns the keys.
				ImportStateVerifyIgnore: []string{"ssh_keys"},
			},
			{
				// Sizes in a form that is not the API's own, and a change of
				// everything that changes in place.
				Config:           e.provider(demoToken) + machine("  cpu = {\n    min = 0.75\n    max = 4\n  }\n  memory = \"4096MiB\"\n  disk = \"20480MiB\"\n  tailscale_auth_key = \"tskey-auth-kSECRETKEY\""),
				ConfigPlanChecks: action(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					vmidIs(&vmid, true),
					resource.TestCheckResourceAttr(web, "cpu.min", "0.75"),
					resource.TestCheckResourceAttr(web, "cpu.max", "4"),
					resource.TestCheckResourceAttr(web, "memory", "4096MiB"),
					resource.TestCheckResourceAttr(web, "disk", "20480MiB"),
					// The machine runs: the API reports what waits for a stop and start.
					resource.TestCheckResourceAttr(web, "pending.cpu_max.running", "2"),
					resource.TestCheckResourceAttr(web, "pending.cpu_max.configured", "4"),
					resource.TestCheckResourceAttr(web, "pending.memory.running", "2GiB"),
					resource.TestCheckResourceAttr(web, "pending.memory.configured", "4GiB"),
					func(*terraform.State) error {
						if m := e.fake.Machine("demo", "web1"); m.Memory.String() != "4GiB" || m.Disk.String() != "20GiB" || m.CPU.Min != 0.75 {
							return fmt.Errorf("the server has %+v", m)
						}
						return nil
					},
				),
			},
			{
				// The tenant stops and starts the machine in PVE: pending goes
				// away, and that is not a change to apply.
				PreConfig:        func() { e.fake.StopStart("demo", "web1") },
				Config:           e.provider(demoToken) + machine("  cpu = {\n    min = 0.75\n    max = 4\n  }\n  memory = \"4GiB\"\n  disk = \"20GiB\""),
				ConfigPlanChecks: action(plancheck.ResourceActionNoop),
				Check:            resource.TestCheckNoResourceAttr(web, "pending.%"),
			},
		},
	})
}

func TestAccMachine_overQuota(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy:             e.gone("web1"),
		Steps: []resource.TestStep{
			{
				Config:      e.provider(demoToken) + machine(`  memory = "32GiB"`),
				ExpectError: regexp.MustCompile(`(?s)over_quota.*memory 32GiB`),
			},
			{
				Config: e.provider(demoToken) + machine(`  memory = "8GiB"`),
				Check:  resource.TestCheckResourceAttr(web, "memory", "8GiB"),
			},
			{
				// Over quota on a change: refused, and the machine is as it was.
				Config:      e.provider(demoToken) + machine(`  memory = "17GiB"`),
				ExpectError: regexp.MustCompile(`over_quota`),
			},
			{
				Config:           e.provider(demoToken) + machine(`  memory = "8GiB"`),
				ConfigPlanChecks: action(plancheck.ResourceActionNoop),
			},
		},
	})
}

func TestAccMachine_noQuota(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      e.provider(otherToken) + machine(""),
			ExpectError: regexp.MustCompile(`no_quota`),
		}},
	})
}

// Another tenant's machine: the path is refused, whatever is done with it.
func TestAccMachine_forbidden(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config:      e.provider(demoToken) + machine(`  tenant = "other"`),
				ExpectError: regexp.MustCompile(`forbidden`),
			},
			{
				Config:      e.provider(demoToken) + "data \"boxbox_machine\" \"other\" {\n  tenant = \"other\"\n  name   = \"web1\"\n}\n",
				ExpectError: regexp.MustCompile(`forbidden`),
			},
			{
				Config:        e.provider(demoToken) + machine(`  tenant = "other"`),
				ResourceName:  web,
				ImportState:   true,
				ImportStateId: "other/web1",
				ExpectError:   regexp.MustCompile(`forbidden`),
			},
			{
				// A tenant may not send the owner's fields.
				Config:      e.provider(demoToken) + machine(`  node = "node2"`),
				ExpectError: regexp.MustCompile(`forbidden`),
			},
		},
	})
}

// A creation that ends in `failed` is an error, the machine is kept in the
// state (it exists and uses quota), and the next apply replaces it.
func TestAccMachine_failedCreation(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	var failedVMID int64
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy:             e.gone("web1"),
		Steps: []resource.TestStep{
			{
				PreConfig:   func() { e.fake.FailCreation("demo", "web1", "importing the disk failed: no space left") },
				Config:      e.provider(demoToken) + machine(""),
				ExpectError: regexp.MustCompile(`(?s)The machine failed.*importing\s+the\s+disk\s+failed:\s+no\s+space\s+left`),
			},
			{
				PreConfig: func() {
					m := e.fake.Machine("demo", "web1")
					if m == nil || m.Phase != "failed" {
						t.Fatalf("the failed machine must still be there, got %+v", m)
					}
					failedVMID = m.VMID
				},
				Config:           e.provider(demoToken) + machine(""),
				ConfigPlanChecks: action(plancheck.ResourceActionReplace),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(web, "phase", "ready"),
					func(*terraform.State) error {
						if m := e.fake.Machine("demo", "web1"); m.VMID == failedVMID {
							return fmt.Errorf("the failed machine was not replaced")
						}
						return nil
					},
				),
			},
		},
	})
}

// The wait is bounded: a creation that does not finish in `timeouts.create`
// is an error, not a hang.
func TestAccMachine_timeout(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	slow := machine("  timeouts = {\n    create = \"300ms\"\n  }")
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy:             e.gone("web1"),
		Steps: []resource.TestStep{
			{
				PreConfig:   func() { e.fake.HoldCreation("demo", "web1") },
				Config:      e.provider(demoToken) + slow,
				ExpectError: regexp.MustCompile(`Timed\s+out\s+waiting\s+for\s+the\s+machine\s+to\s+be\s+ready`),
			},
			{
				PreConfig: func() {
					if m := e.fake.Machine("demo", "web1"); m == nil || m.Phase != "creating" {
						t.Fatalf("the machine must still be creating, got %+v", m)
					}
				},
				Config:           e.provider(demoToken) + slow,
				ConfigPlanChecks: action(plancheck.ResourceActionReplace),
				Check:            resource.TestCheckResourceAttr(web, "phase", "ready"),
			},
		},
	})
}

func TestAccMachine_deleteTimeout(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{DestroyPolls: 1 << 20})
	cfg := e.provider(demoToken) + machine("  timeouts = {\n    delete = \"300ms\"\n  }")
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cfg},
			{Config: cfg, Destroy: true, ExpectError: regexp.MustCompile(`Timed\s+out\s+waiting\s+for\s+the\s+machine\s+to\s+be\s+gone`)},
			// Let the destroy at the end of the test find it gone.
			{PreConfig: func() { e.fake.Remove("demo", "web1") }, Config: e.provider(demoToken) + `data "boxbox_me" "me" {}`},
		},
	})
}

// A machine deleted behind Terraform's back is noticed and made again.
func TestAccMachine_drift(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	var vmid string
	cfg := e.provider(demoToken) + machine("")
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy:             e.gone("web1"),
		Steps: []resource.TestStep{
			{Config: cfg, Check: vmidIs(&vmid, true)},
			{
				PreConfig:          func() { e.fake.Remove("demo", "web1") },
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config:           cfg,
				ConfigPlanChecks: action(plancheck.ResourceActionCreate),
				Check:            resource.ComposeAggregateTestCheckFunc(vmidIs(&vmid, false), resource.TestCheckResourceAttr(web, "phase", "ready")),
			},
			{
				// Drift of a value: somebody grew the disk elsewhere. The
				// configuration is smaller, the API will not shrink.
				PreConfig: func() {
					if _, err := e.put("web1", `{"image":"ubuntu-24.04","disk":"30GiB"}`); err != nil {
						t.Fatal(err)
					}
				},
				Config:      cfg,
				ExpectError: regexp.MustCompile(`cannot_shrink`),
			},
			{Config: e.provider(demoToken) + machine(`  disk = "30GiB"`), ConfigPlanChecks: action(plancheck.ResourceActionNoop)},
		},
	})
}

func TestAccMachine_ha(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	var vmid string
	ha := func(n int) string {
		return e.provider(demoToken) + machine(fmt.Sprintf("  ha = {\n    replicas = %d\n  }", n))
	}
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy:             e.gone("web1"),
		Steps: []resource.TestStep{
			{
				Config: ha(1),
				Check: resource.ComposeAggregateTestCheckFunc(
					vmidIs(&vmid, true),
					resource.TestCheckResourceAttr(web, "ha.replicas", "1"),
					resource.TestCheckResourceAttr(web, "ha.restart", "true"),
					resource.TestCheckResourceAttr(web, "ha_nodes.#", "1"),
					resource.TestCheckResourceAttr(web, "ha_protected", "true"),
					func(*terraform.State) error {
						if got := e.lastPut("web1")["ha"]; fmt.Sprint(got) != "map[replicas:1]" {
							return fmt.Errorf("a tenant's ha was sent as %v", got)
						}
						return nil
					},
				),
			},
			{ResourceName: web, ImportState: true, ImportStateId: "demo/web1", ImportStateVerify: true, ImportStateVerifyIgnore: []string{"ssh_keys"}},
			{
				Config:           ha(2),
				ConfigPlanChecks: action(plancheck.ResourceActionUpdate),
				Check:            resource.ComposeAggregateTestCheckFunc(vmidIs(&vmid, true), resource.TestCheckResourceAttr(web, "ha_nodes.#", "2")),
			},
			{Config: ha(1), ExpectError: regexp.MustCompile(`cannot_reduce_ha`)},
			{Config: e.provider(demoToken) + machine(""), ExpectError: regexp.MustCompile(`cannot_reduce_ha`)},
			{Config: ha(3), ExpectError: regexp.MustCompile(`ha.replicas: must be 1-2`)},
			{Config: ha(2), ConfigPlanChecks: action(plancheck.ResourceActionNoop)},
		},
	})
}

// The console password is write-only: set at creation and whenever
// password_version changes, never in the state, never a diff by itself.
func TestAccMachine_password(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	pw := func(password string, version int) string {
		return e.provider(demoToken) + machine(fmt.Sprintf("  password = %q\n  password_version = %d", password, version))
	}
	is := func(want string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			if got := e.fake.Password("demo", "web1"); got != want {
				return fmt.Errorf("the machine's password is %q, want %q", got, want)
			}
			return nil
		}
	}
	null := []statecheck.StateCheck{statecheck.ExpectKnownValue(web, tfjsonpath.New("password"), knownvalue.Null())}
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy:             e.gone("web1"),
		Steps: []resource.TestStep{
			{Config: pw("first password", 1), Check: is("first password"), ConfigStateChecks: null},
			// A new password alone is not a change: nothing to compare it with.
			{Config: pw("second password", 1), ConfigPlanChecks: action(plancheck.ResourceActionNoop), Check: is("first password")},
			{Config: pw("second password", 2), ConfigPlanChecks: action(plancheck.ResourceActionUpdate), Check: is("second password"), ConfigStateChecks: null},
			{
				// A stopped machine cannot take a password: the API says so.
				PreConfig:   func() { e.fake.Stop("demo", "web1") },
				Config:      pw("third password", 3),
				ExpectError: regexp.MustCompile(`not_ready`),
			},
		},
	})
}

// What is used only at creation: a change of ssh_keys is taken into the
// state without touching the machine; a change of image replaces it.
func TestAccMachine_createOnlyFields(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	var vmid string
	with := func(image, key string) string {
		return e.provider(demoToken) + fmt.Sprintf("resource \"boxbox_machine\" \"web\" {\n  name = \"web1\"\n  image = %q\n  ssh_keys = [%q]\n}\n", image, key)
	}
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy:             e.gone("web1"),
		Steps: []resource.TestStep{
			{Config: with("ubuntu-24.04", sshKey), Check: vmidIs(&vmid, true)},
			{
				Config:           with("ubuntu-24.04", otherSSHKey),
				ConfigPlanChecks: action(plancheck.ResourceActionUpdate),
				Check:            resource.ComposeAggregateTestCheckFunc(vmidIs(&vmid, true), resource.TestCheckResourceAttr(web, "ssh_keys.0", otherSSHKey)),
			},
			{Config: with("ubuntu-24.04", otherSSHKey), ConfigPlanChecks: action(plancheck.ResourceActionNoop)},
			{
				Config:           with("debian-13", otherSSHKey),
				ConfigPlanChecks: action(plancheck.ResourceActionReplace),
				Check:            resource.ComposeAggregateTestCheckFunc(vmidIs(&vmid, false), resource.TestCheckResourceAttr(web, "image", "debian-13")),
			},
			{
				Config:           e.provider(demoToken) + "resource \"boxbox_machine\" \"web\" {\n  name = \"web2\"\n  image = \"debian-13\"\n  ssh_keys = [\"" + otherSSHKey + "\"]\n}\n",
				ConfigPlanChecks: action(plancheck.ResourceActionReplace),
				Check:            resource.TestCheckResourceAttr(web, "id", "demo/web2"),
			},
		},
	})
}

func TestAccMachine_validation(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	bad := func(body, want string) resource.TestStep {
		return resource.TestStep{
			Config:      e.provider(demoToken) + "resource \"boxbox_machine\" \"web\" {\n" + body + "\n}\n",
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(want),
		}
	}
	keys := fmt.Sprintf("  ssh_keys = [%q]\n", sshKey)
	ok := "  name = \"web1\"\n  image = \"ubuntu-24.04\"\n" + keys
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			bad("  name = \"Web_1\"\n  image = \"ubuntu-24.04\"\n"+keys, `a-z,\s+0-9`),
			bad("  name = \"web1\"\n"+keys, `Missing\s+image`),
			bad("  name = \"web1\"\n  image = \"ubuntu-24.04\"", `Missing\s+ssh_keys`),
			bad("  name = \"web1\"\n  image = \"ubuntu-24.04\"\n  ssh_keys = [\"notakey\"]", `OpenSSH\s+public\s+key`),
			bad(ok+"  memory = \"4G\"", `Not\s+a\s+size`),
			bad(ok+"  disk = \"2GiB\"", `at\s+least\s+4GiB`),
			bad(ok+"  disk = \"5000MiB\"", `whole\s+gibibytes`),
			bad(ok+"  cpu = {\n    min = 0.07\n  }", `steps\s+of\s+0.05`),
			bad(ok+"  cpu = {\n    min = 3\n  }", `cannot\s+be\s+more\s+than`),
			bad(ok+"  cpu = {\n    max = 0\n  }", `between\s+1\s+and\s+512`),
			bad(ok+"  ha = {}", `Missing\s+replicas`),
			bad(ok+"  ha = {\n    nodes = [\"node2\"]\n  }", `needs\s+.node`),
			bad(ok+"  tenant = \"admin!\"", `2-8\s+characters`),
			bad(ok+"  tailscale_auth_key = \"has space\"", `A-Z\s+a-z\s+0-9`),
			bad(ok+"  password_version = 1", `password`),
			bad(ok+"  password = \"short\"\n  password_version = 1", `8-128`),
			bad(ok+"  iso = \"debian.iso\"", `Both\s+image\s+and\s+iso`),
			bad(ok+"  user = \"root\"", `root`),
		},
	})
}

func TestAccMachine_importRefusals(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	e.fake.AddConfigMachine("demo", "hub", cpu(1, 2), gib(2), gib(10))
	cfg := e.provider(demoToken) + machine("")
	step := func(id, want string) resource.TestStep {
		return resource.TestStep{Config: cfg, ResourceName: web, ImportState: true, ImportStateId: id, ExpectError: regexp.MustCompile(want)}
	}
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			// A tenant cannot adopt, with or without asking for it.
			step("demo/hub", `Ask\s+the\s+owner\s+to\s+adopt`),
			step("demo/hub:adopt", `Ask\s+the\s+owner\s+to\s+adopt`),
			{
				// The owner must ask for it: an import alone changes nothing.
				Config: e.provider(adminToken) + machine(`  tenant = "demo"`), ResourceName: web, ImportState: true, ImportStateId: "demo/hub",
				ExpectError: regexp.MustCompile(`demo/hub:adopt`),
			},
			{
				PreConfig: func() {
					if m := e.fake.Machine("demo", "hub"); m.Source != "config" {
						t.Fatalf("a refused import adopted the machine: %+v", m)
					}
				},
				Config: e.provider(demoToken) + `data "boxbox_me" "me" {}`,
			},
			step("demo/nothing", `not_found`),
			step("web1", `Invalid\s+import\s+ID`),
			step("admin/web1", `Invalid\s+import\s+ID`),
		},
	})
}

// The owner imports a machine made by the owner's config with `:adopt`: the
// import adopts it, and from then on the resource manages it in place.
func TestAccMachine_importAdopts(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	e.fake.AddConfigMachine("demo", "hub", cpu(1, 2), gib(2), gib(10))
	vmid := e.fake.Machine("demo", "hub").VMID
	hub := func(memory string) string {
		return e.provider(adminToken) + fmt.Sprintf("resource \"boxbox_machine\" \"web\" {\n  tenant = \"demo\"\n  name = \"hub\"\n  image = \"debian-13\"\n  ssh_keys = [%q]\n  cpu = {\n    min = 1\n    max = 2\n  }\n  memory = %q\n}\n", sshKey, memory)
	}
	same := func(*terraform.State) error {
		if m := e.fake.Machine("demo", "hub"); m == nil || m.VMID != vmid || m.Source != "api" {
			return fmt.Errorf("the adopted machine must be the same machine, now the API's: %+v", m)
		}
		return nil
	}
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy: func(*terraform.State) error {
			if m := e.fake.Machine("demo", "hub"); m != nil {
				return fmt.Errorf("the adopted machine was not destroyed with the resource: %+v", m)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: hub("2GiB"), ResourceName: web, ImportState: true, ImportStateId: "demo/hub:adopt", ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("%d states", len(states))
					}
					a := states[0].Attributes
					if a["id"] != "demo/hub" || a["source"] != "api" || a["memory"] != "2GiB" || a["image"] != "" {
						return fmt.Errorf("the imported state is %v", a)
					}
					return same(nil)
				},
			},
			{
				// The image of an adopted machine is not known to the API: the
				// configured one is recorded, and the machine is not replaced.
				Config:           hub("2GiB"),
				ConfigPlanChecks: action(plancheck.ResourceActionUpdate),
				Check:            resource.ComposeAggregateTestCheckFunc(same, resource.TestCheckResourceAttr(web, "image", "debian-13")),
			},
			{Config: hub("2GiB"), ConfigPlanChecks: action(plancheck.ResourceActionNoop)},
			{
				Config:           hub("4GiB"),
				ConfigPlanChecks: action(plancheck.ResourceActionUpdate),
				Check:            resource.ComposeAggregateTestCheckFunc(same, resource.TestCheckResourceAttr(web, "memory", "4GiB")),
			},
		},
	})
}

// Create must not quietly take over a machine that is already there.
func TestAccMachine_alreadyExists(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	if _, err := e.put("web1", fmt.Sprintf(`{"image":"ubuntu-24.04","ssh_keys":[%q]}`, sshKey)); err != nil {
		t.Fatal(err)
	}
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: e.provider(demoToken) + machine(`  memory = "4GiB"`), ExpectError: regexp.MustCompile(`already\s+exists`)},
			{
				PreConfig: func() {
					if m := e.fake.Machine("demo", "web1"); m.Memory.String() != "2GiB" {
						t.Fatalf("the existing machine was changed: %+v", m)
					}
				},
				Config: e.provider(demoToken) + `data "boxbox_me" "me" {}`,
			},
		},
	})
}

// A passing 5xx is sent again: every write of the API is idempotent.
func TestAccMachine_retriesPassingFailures(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy:             e.gone("web1"),
		Steps: []resource.TestStep{{
			PreConfig: func() {
				e.fake.Inject(503, "locked", "0")
				e.fake.Inject(502, "unavailable", "")
			},
			Config: e.provider(demoToken) + machine(""),
			Check:  resource.TestCheckResourceAttr(web, "phase", "ready"),
		}},
	})
}

// The owner's fields, against the shape the contract specifies.
func TestAccMachine_ownerFields(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{AdminImplemented: true})
	var vmid string
	owner := func(extra string) string {
		return e.provider(adminToken) + machine("  tenant = \"demo\"\n  node = \"node2\"\n  user = \"ops\"\n  cloud_init = \"#cloud-config\\npackages: [htop]\\n\"\n  ha = {\n    nodes = [\"node3\"]\n    restart = false\n  }\n"+extra)
	}
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy:             e.gone("web1"),
		Steps: []resource.TestStep{
			{
				Config: owner(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					vmidIs(&vmid, true),
					resource.TestCheckResourceAttr(web, "node", "node2"),
					resource.TestCheckResourceAttr(web, "current_node", "node2"),
					resource.TestCheckResourceAttr(web, "user", "ops"),
					resource.TestCheckResourceAttr(web, "ha.replicas", "1"),
					resource.TestCheckResourceAttr(web, "ha.nodes.#", "1"),
					resource.TestCheckResourceAttr(web, "ha.restart", "false"),
					resource.TestCheckResourceAttr(web, "ha_nodes.0", "node3"),
					resource.TestCheckResourceAttr(web, "ha_protected", "false"),
					func(*terraform.State) error {
						put := e.lastPut("web1")
						if put["cloud_init"] != "#cloud-config\npackages: [htop]\n" || put["node"] != "node2" || put["user"] != "ops" {
							return fmt.Errorf("the PUT was %v", put)
						}
						if fmt.Sprint(put["ha"]) != "map[nodes:[node3] restart:false]" {
							return fmt.Errorf("ha was sent as %v", put["ha"])
						}
						return nil
					},
				),
				ConfigStateChecks: []statecheck.StateCheck{statecheck.ExpectKnownValue(web, tfjsonpath.New("cloud_init"), knownvalue.Null())},
			},
			{Config: owner(`  memory = "4GiB"`), ConfigPlanChecks: action(plancheck.ResourceActionUpdate), Check: vmidIs(&vmid, true)},
			{
				// `node` is where the machine was made; a new value does not move it.
				Config:           strings.Replace(owner(`  memory = "4GiB"`), `node = "node2"`, `node = "node1"`, 1),
				ConfigPlanChecks: action(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(vmidIs(&vmid, true),
					resource.TestCheckResourceAttr(web, "node", "node1"), resource.TestCheckResourceAttr(web, "current_node", "node2")),
			},
		},
	})
}

func TestAccMachine_ownerISO(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{AdminImplemented: true})
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: e.provider(adminToken) + "resource \"boxbox_machine\" \"web\" {\n  tenant = \"demo\"\n  name = \"web1\"\n  iso = \"debian-13.iso\"\n}\n",
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(web, "status", "stopped"),
				resource.TestCheckNoResourceAttr(web, "image"),
				resource.TestCheckNoResourceAttr(web, "user"),
			),
		}},
	})
}

// Today the server answers 501 to the owner's fields: a clear diagnostic.
func TestAccMachine_ownerFieldsNotImplemented(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config:      e.provider(adminToken) + machine("  tenant = \"demo\"\n  node = \"node2\""),
				ExpectError: regexp.MustCompile(`the\s+server\s+does\s+not\s+implement\s+this\s+yet`),
			},
			{
				Config:      e.provider(adminToken) + machine(""),
				ExpectError: regexp.MustCompile(`Missing\s+tenant`),
			},
			{
				// Without the owner's fields the owner creates machines today.
				Config: e.provider(adminToken) + machine(`  tenant = "demo"`),
				Check:  resource.TestCheckResourceAttr(web, "user", "demo"),
			},
		},
	})
}
