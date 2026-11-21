package provider

import (
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/triac-hq/terraform-provider-boxbox/internal/client"
	"github.com/triac-hq/terraform-provider-boxbox/internal/fakeapi"
)

// The acceptance tests run the real CLI (OpenTofu, or Terraform when
// TF_ACC_TERRAFORM_PATH says so) against the in-process fake of the API.
// Nothing outside this process is reached, so they run by default whenever
// `tofu` is installed.
func TestMain(m *testing.M) {
	if os.Getenv("TF_ACC_TERRAFORM_PATH") == "" {
		if tofu, err := exec.LookPath("tofu"); err == nil {
			os.Setenv("TF_ACC_TERRAFORM_PATH", tofu)
			os.Setenv("TF_ACC_PROVIDER_NAMESPACE", "hashicorp")
			os.Setenv("TF_ACC_PROVIDER_HOST", "registry.opentofu.org")
		}
	}
	if os.Getenv("TF_ACC_TERRAFORM_PATH") != "" && os.Getenv("TF_ACC") == "" {
		os.Setenv("TF_ACC", "1")
	}
	// The environment must not configure the provider behind a test's back.
	for _, k := range []string{EnvEndpoint, EnvAPIToken, EnvAccessClientID, EnvAccessClientSecret} {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

const (
	demoSecret  = "01234567-89ab-cdef-0123-456789abcdef"
	demoToken   = "demo@pve!tf=" + demoSecret
	otherToken  = "other@pve!tf=11111111-89ab-cdef-0123-456789abcdef"
	adminToken  = "admin@pve!tf=22222222-89ab-cdef-0123-456789abcdef"
	sshKey      = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIabc demo@laptop"
	otherSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIxyz demo@desktop"
)

type testEnv struct {
	t    *testing.T
	fake *fakeapi.Server
	url  string
}

func gib(n int64) client.Size { return client.SizeFromMiB(n * 1024) }

// newTestEnv starts a fake with tenant `demo` (a quota of 3 machines, 16GiB,
// HA allowed), tenant `other` (no quota) and the owner.
func newTestEnv(t *testing.T, opts fakeapi.Options) *testEnv {
	t.Helper()
	if opts.CreatePolls == 0 {
		opts.CreatePolls = 2
	}
	if opts.DestroyPolls == 0 {
		opts.DestroyPolls = 1
	}
	fake := fakeapi.New(opts)
	fake.AddTenant("demo", &client.Quota{Machines: 3, CPU: client.CPU{Min: 4, Max: 16}, Memory: gib(16), Disk: gib(100), HA: true})
	fake.AddTenant("other", nil)
	fake.AddToken(demoToken, false)
	fake.AddToken(otherToken, false)
	fake.AddToken(adminToken, true)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return &testEnv{t: t, fake: fake, url: srv.URL}
}

func (e *testEnv) provider(token string) string {
	return fmt.Sprintf("provider \"boxbox\" {\n  endpoint  = %q\n  api_token = %q\n}\n", e.url, token)
}

func factories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"boxbox": providerserver.NewProtocol6WithError(New("test", WithTiming(5*time.Millisecond, 5*time.Millisecond))()),
	}
}

// gone checks that the destroy at the end of a test left nothing of the
// tenant's API machines behind.
func (e *testEnv) gone(names ...string) func(*terraform.State) error {
	return func(*terraform.State) error {
		for _, n := range names {
			if m := e.fake.Machine("demo", n); m != nil {
				return fmt.Errorf("demo/%s is still there (phase %s)", n, m.Phase)
			}
		}
		return nil
	}
}

func TestAccProvider_environmentAndAccess(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{Access: &fakeapi.Access{ID: "id.access", Secret: "edge-secret-value"}})
	t.Setenv(EnvEndpoint, e.url)
	t.Setenv(EnvAPIToken, demoToken)
	t.Setenv(EnvAccessClientID, "id.access")
	t.Setenv(EnvAccessClientSecret, "edge-secret-value")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: `data "boxbox_me" "me" {}`,
				Check:  resource.TestCheckResourceAttr("data.boxbox_me.me", "user", "demo@pve"),
			},
			{
				// A wrong service token never reaches the API: the edge answers.
				Config:      "provider \"boxbox\" {\n access_client_secret = \"wrong\"\n}\n" + `data "boxbox_me" "me" {}`,
				ExpectError: regexp.MustCompile(`Cloudflare\s+Access`),
			},
		},
	})
}

func TestAccProvider_invalidConfiguration(t *testing.T) {
	e := newTestEnv(t, fakeapi.Options{})
	me := `data "boxbox_me" "me" {}`
	resource.ParallelTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: "provider \"boxbox\" {}\n" + me, ExpectError: regexp.MustCompile(`Missing\s+endpoint`)},
			{Config: fmt.Sprintf("provider \"boxbox\" {\n endpoint = %q\n api_token = \"nonsense-%s\"\n}\n", e.url, demoSecret) + me, ExpectError: regexp.MustCompile(`not\s+a\s+PVE\s+API\s+token`)},
			{Config: "provider \"boxbox\" {\n endpoint = \"http://cluster.example.com\"\n api_token = \"" + demoToken + "\"\n}\n" + me, ExpectError: regexp.MustCompile(`must\s+be\s+https`)},
			{Config: e.provider("demo@pve!tf=ffffffff-0000-0000-0000-000000000000") + me, ExpectError: regexp.MustCompile(`unauthenticated`)},
		},
	})
}
