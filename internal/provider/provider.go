// Package provider is the Terraform and OpenTofu provider `boxbox`.
package provider

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/triac-hq/terraform-provider-boxbox/internal/client"
)

// Environment variables that set the provider's configuration.
const (
	EnvEndpoint           = "BOXBOX_ENDPOINT"
	EnvAPIToken           = "BOXBOX_API_TOKEN"
	EnvAccessClientID     = "BOXBOX_ACCESS_CLIENT_ID"
	EnvAccessClientSecret = "BOXBOX_ACCESS_CLIENT_SECRET"
)

// Option changes how the provider's client behaves. Tests use it to keep
// waits short; the released provider uses none.
type Option func(*client.Config)

// WithTiming sets the client's shortest poll wait and first retry wait.
func WithTiming(minPoll, backoff time.Duration) Option {
	return func(c *client.Config) { c.MinPoll, c.Backoff = minPoll, backoff }
}

// New makes the provider.
func New(version string, opts ...Option) func() provider.Provider {
	return func() provider.Provider { return &boxboxProvider{version: version, opts: opts} }
}

type boxboxProvider struct {
	version string
	opts    []Option
}

type providerModel struct {
	Endpoint           types.String `tfsdk:"endpoint"`
	APIToken           types.String `tfsdk:"api_token"`
	AccessClientID     types.String `tfsdk:"access_client_id"`
	AccessClientSecret types.String `tfsdk:"access_client_secret"`
}

func (p *boxboxProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "boxbox"
	resp.Version = p.version
}

func (p *boxboxProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Machines of a tenant on a boxbox3 cluster (Proxmox VE), through boxbox3's HTTP API.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Address of the cluster's web interface, like `https://cluster.example.com` (scheme and host only). Environment: `" + EnvEndpoint + "`.",
			},
			"api_token": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "PVE API token of the tenant (or the owner): `<user>@pve!<tokenid>=<secret>`. Environment: `" + EnvAPIToken + "`.",
			},
			"access_client_id": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Client ID of the Cloudflare Access service token that lets automation through the edge. Environment: `" + EnvAccessClientID + "`.",
			},
			"access_client_secret": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Client secret of the Cloudflare Access service token. Environment: `" + EnvAccessClientSecret + "`.",
			},
		},
	}
}

func (p *boxboxProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var m providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pick := func(v types.String, attr, env string, required bool) string {
		if v.IsUnknown() {
			resp.Diagnostics.AddAttributeError(path.Root(attr), "Value not known yet",
				"The provider cannot be configured from a value that is known only after apply. Set `"+attr+"` to a known value or use the environment variable "+env+".")
			return ""
		}
		s := v.ValueString()
		if v.IsNull() {
			s = os.Getenv(env)
		}
		if s == "" && required {
			resp.Diagnostics.AddAttributeError(path.Root(attr), "Missing "+attr,
				"Set `"+attr+"` in the provider block or the environment variable "+env+".")
		}
		return s
	}
	cfg := client.Config{
		Endpoint:           pick(m.Endpoint, "endpoint", EnvEndpoint, true),
		APIToken:           pick(m.APIToken, "api_token", EnvAPIToken, true),
		AccessClientID:     pick(m.AccessClientID, "access_client_id", EnvAccessClientID, false),
		AccessClientSecret: pick(m.AccessClientSecret, "access_client_secret", EnvAccessClientSecret, false),
		UserAgent:          "terraform-provider-boxbox/" + p.version,
	}
	if resp.Diagnostics.HasError() {
		return
	}
	for _, o := range p.opts {
		o(&cfg)
	}
	c, err := client.New(cfg)
	if err != nil {
		// client.New never puts a secret into its errors.
		resp.Diagnostics.AddError("Invalid provider configuration", err.Error())
		return
	}
	resp.ResourceData = c
	resp.DataSourceData = c
}

func (p *boxboxProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{newMachineResource, newTenantResource}
}

func (p *boxboxProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{newMeDataSource, newMachineDataSource, newMachinesDataSource}
}

// clientFrom takes the client out of what Configure handed over.
func clientFrom(data any, diags *diag.Diagnostics) *client.Client {
	if data == nil {
		return nil
	}
	c, ok := data.(*client.Client)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", data))
		return nil
	}
	return c
}

// hints say what to do about an error code of the contract.
var hints = map[string]string{
	"unauthenticated":     "PVE did not accept the API token: check api_token (the form is <user>@pve!<tokenid>=<secret>) and that the token has not been removed or expired.",
	"forbidden":           "The token's user is not allowed to do this: a tenant reaches only its own machines and may not set the owner's fields (node, user, cloud_init, iso, ha.nodes, ha.restart).",
	"no_quota":            "The tenant has no quota, so it cannot create or change machines. The owner sets the quota.",
	"over_quota":          "The request would take the tenant's machines over its quota. The `boxbox_me` data source shows the quota and the usage.",
	"no_room":             "The machine (or its replicas) does not fit on any node with the guarantees kept.",
	"not_api_machine":     "The machine is made by the owner's boxbox3 config: the API reads it but never changes or destroys it. Use the `boxbox_machine` data source for it.",
	"busy":                "The machine is still being created; a change is accepted once its phase is `ready`.",
	"destroying":          "The machine is being destroyed; wait until it is gone.",
	"cannot_shrink":       "A disk never shrinks. To get a smaller disk, replace the machine (`-replace`).",
	"cannot_reduce_ha":    "Replicas are never removed through the API. Replace the machine (`-replace`) or ask the owner.",
	"image_differs":       "The image of an existing machine cannot change.",
	"not_ready":           "The machine must be running with phase `ready`, and its guest agent must answer.",
	"tenant_not_ready":    "The tenant's pool, network or permissions are not complete: the owner's `boxbox3 apply` has not finished.",
	"tenant_has_machines": "A tenant with machines is not deleted: destroy its machines first.",
	"locked":              "Another change held the cluster's reservation lock for too long. Run again.",
	"timeout":             "The request took too long on the server. The change may still have happened: run plan again to see.",
}

// addAPIError turns an error of the client into a diagnostic. The client has
// already taken the secrets out of the text.
func addAPIError(diags *diag.Diagnostics, summary string, err error) {
	if client.IsNotImplemented(err) {
		diags.AddError(summary+": the server does not implement this yet",
			"The boxbox3 API answered 501 not_implemented. Tenants (`boxbox_tenant`) and the owner's machine fields "+
				"(node, user, cloud_init, iso, ha.nodes, ha.restart) are specified in the API contract but the server does not have them yet. "+
				"Nothing was changed.\n\n"+err.Error())
		return
	}
	detail := err.Error()
	var ae *client.APIError
	if asAPIError(err, &ae) {
		if h, ok := hints[ae.Code]; ok {
			detail += "\n\n" + h
		}
	}
	diags.AddError(summary, detail)
}
