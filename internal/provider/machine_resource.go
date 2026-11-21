package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/triac-hq/terraform-provider-boxbox/internal/client"
)

// The contract asks a client to wait at least 40 min for a creation and
// 15 min for a destruction.
const (
	defaultCreateTimeout = 45 * time.Minute
	defaultUpdateTimeout = 45 * time.Minute
	defaultDeleteTimeout = 20 * time.Minute
)

var (
	_ resource.Resource                   = (*machineResource)(nil)
	_ resource.ResourceWithConfigure      = (*machineResource)(nil)
	_ resource.ResourceWithImportState    = (*machineResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*machineResource)(nil)
	_ resource.ResourceWithValidateConfig = (*machineResource)(nil)
)

func newMachineResource() resource.Resource { return &machineResource{} }

type machineResource struct{ client *client.Client }

type machineModel struct {
	ID               types.String   `tfsdk:"id"`
	Tenant           types.String   `tfsdk:"tenant"`
	Name             types.String   `tfsdk:"name"`
	Image            types.String   `tfsdk:"image"`
	CPU              types.Object   `tfsdk:"cpu"`
	Memory           SizeValue      `tfsdk:"memory"`
	Disk             SizeValue      `tfsdk:"disk"`
	HA               types.Object   `tfsdk:"ha"`
	SSHKeys          types.List     `tfsdk:"ssh_keys"`
	TailscaleAuthKey types.String   `tfsdk:"tailscale_auth_key"`
	Password         types.String   `tfsdk:"password"`
	PasswordVersion  types.Int64    `tfsdk:"password_version"`
	Node             types.String   `tfsdk:"node"`
	User             types.String   `tfsdk:"user"`
	CloudInit        types.String   `tfsdk:"cloud_init"`
	ISO              types.String   `tfsdk:"iso"`
	VMID             types.Int64    `tfsdk:"vmid"`
	Source           types.String   `tfsdk:"source"`
	Phase            types.String   `tfsdk:"phase"`
	Reason           types.String   `tfsdk:"reason"`
	CurrentNode      types.String   `tfsdk:"current_node"`
	Status           types.String   `tfsdk:"status"`
	HANodes          types.List     `tfsdk:"ha_nodes"`
	HAProtected      types.Bool     `tfsdk:"ha_protected"`
	Addresses        types.Object   `tfsdk:"addresses"`
	Pending          types.Object   `tfsdk:"pending"`
	Timeouts         timeouts.Value `tfsdk:"timeouts"`
}

// machineSettable are the attributes a configuration can set and the state
// keeps (write-only ones are never in either).
var machineSettable = []string{
	"tenant", "name", "image", "cpu", "memory", "disk", "ha", "ssh_keys",
	"password_version", "node", "user", "iso", "timeouts",
}

type haModel struct {
	Replicas types.Int64 `tfsdk:"replicas"`
	Nodes    types.Set   `tfsdk:"nodes"`
	Restart  types.Bool  `tfsdk:"restart"`
}

var haAttrTypes = map[string]attr.Type{
	"replicas": types.Int64Type,
	"nodes":    types.SetType{ElemType: types.StringType},
	"restart":  types.BoolType,
}

// adoptSuffix on an import ID asks the owner's import to adopt a machine made
// by the owner's config. It is opt-in because it changes the cluster.
const adoptSuffix = ":adopt"

var (
	sshKeyRe    = regexp.MustCompile(`^\S+ [A-Za-z0-9+/]+=*( [^\r\n]*)?$`)
	tailscaleRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)
	passwordRe  = regexp.MustCompile(`^[\x20-\x7e]{8,128}$`)
	loginRe     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

func (r *machineResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_machine"
}

func (r *machineResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *machineResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A virtual machine of a tenant. Creation and destruction are asynchronous on the server: the provider polls until the machine is `ready`, or gone.\n\n" +
			"The API never restarts a machine: a change of `cpu.max` or `memory` is set at once but the running machine gets it when it is next stopped and started from PVE (see `pending`).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<tenant>/<name>`; also the import ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"tenant": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Tenant that owns the machine. Defaults to the tenant of the provider's API token; required with the owner's token. Changing it replaces the machine.",
				Validators:          []validator.String{stringvalidator.RegexMatches(tenantNameRe, tenantNameMsg)},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Name and hostname of the machine, unique within the tenant. Changing it replaces the machine.",
				Validators:          []validator.String{stringvalidator.RegexMatches(machineNameRe, machineNameMsg)},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"image": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Image the machine is created from: one of `images` of the `boxbox_me` data source. Required unless `iso` is set. Changing it replaces the machine (except on an adopted machine whose image the API does not know: there the configured value is only recorded).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplaceIf(
					func(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
						resp.RequiresReplace = !req.StateValue.IsNull()
					},
					"Replaces the machine when the image it was created from changes.",
					"Replaces the machine when the image it was created from changes.",
				)},
			},
			"cpu": schema.SingleNestedAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Threads of the machine.",
				Default: objectdefault.StaticValue(types.ObjectValueMust(cpuAttrTypes, map[string]attr.Value{
					"min": types.Float64Value(0), "max": types.Int64Value(2),
				})),
				Attributes: map[string]schema.Attribute{
					"min": schema.Float64Attribute{
						Optional: true, Computed: true,
						Default:             float64default.StaticFloat64(0),
						MarkdownDescription: "Guaranteed threads, in steps of 0.05, from 0 to `max` (at most 100). Default `0`. A change takes effect at once.",
						Validators:          []validator.Float64{cpuMinValidator{}},
					},
					"max": schema.Int64Attribute{
						Optional: true, Computed: true,
						Default:             int64default.StaticInt64(2),
						MarkdownDescription: "Threads the machine sees, 1-512. Default `2`. A change reaches a running machine at its next stop and start from PVE.",
						Validators:          []validator.Int64{int64validator.Between(1, 512)},
					},
				},
			},
			"memory": schema.StringAttribute{
				CustomType: SizeType{},
				Optional:   true, Computed: true,
				Default:             stringdefault.StaticString("2GiB"),
				MarkdownDescription: "Memory: an integer and `MiB`, `GiB` or `TiB` (`\"4GiB\"`). Default `2GiB`. A change reaches a running machine at its next stop and start from PVE.",
				Validators:          []validator.String{sizeValidator{}},
				PlanModifiers:       []planmodifier.String{sameSize{}},
			},
			"disk": schema.StringAttribute{
				CustomType: SizeType{},
				Optional:   true, Computed: true,
				Default:             stringdefault.StaticString("10GiB"),
				MarkdownDescription: "Disk: whole gibibytes, at least `4GiB`. Default `10GiB`. Grows at once (growing the file system is the machine's business); never shrinks.",
				Validators:          []validator.String{sizeValidator{wholeGiB: true, minMiB: 4096}},
				PlanModifiers:       []planmodifier.String{sameSize{}},
			},
			"ha": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Replicas on other nodes and restart after the loss of a node. Needs `quota.ha`. Replicas are never removed through the API.",
				Attributes: map[string]schema.Attribute{
					"replicas": schema.Int64Attribute{
						Optional: true, Computed: true,
						MarkdownDescription: "Number of replicas, 1 to (number of nodes - 1). boxbox3 picks the nodes. Can grow, cannot shrink. Required unless `nodes` is set.",
						Validators:          []validator.Int64{int64validator.AtLeast(1)},
						PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
					},
					"nodes": schema.SetAttribute{
						ElementType:         types.StringType,
						Optional:            true,
						MarkdownDescription: "Owner only (the server does not implement it yet): the nodes of the replicas by name, instead of `replicas`. Needs `node`.",
						Validators:          []validator.Set{setvalidator.SizeAtLeast(1)},
					},
					"restart": schema.BoolAttribute{
						Optional: true, Computed: true,
						MarkdownDescription: "Owner only (the server does not implement it yet): `false` makes replicas without an HA resource. Default `true`.",
						PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
					},
				},
			},
			"ssh_keys": schema.ListAttribute{
				ElementType:         types.StringType,
				Optional:            true,
				MarkdownDescription: "1-16 OpenSSH public keys for the machine's login user. Required unless `iso` is set. Used only when the machine is created: the API never returns them, and a later change is recorded in the state with a warning but does not reach the machine.",
				Validators: []validator.List{
					listvalidator.SizeBetween(1, 16),
					listvalidator.ValueStringsAre(stringvalidator.LengthAtMost(2048), stringvalidator.RegexMatches(sshKeyRe, "must be an OpenSSH public key: <type> <base64> [comment]")),
				},
			},
			"tailscale_auth_key": schema.StringAttribute{
				Optional: true, Sensitive: true, WriteOnly: true,
				MarkdownDescription: "Tailscale auth key: the machine joins the tenant's own tailnet at first boot. Use a single-use key. Write-only: used only when the machine is created, never stored in the state, never returned by the API.",
				Validators:          []validator.String{stringvalidator.RegexMatches(tailscaleRe, "must be 1-200 characters of A-Z a-z 0-9 - _")},
			},
			"password": schema.StringAttribute{
				Optional: true, Sensitive: true, WriteOnly: true,
				MarkdownDescription: "Password of the login user for the PVE console (SSH does not accept it): 8-128 printable ASCII characters. Write-only: never stored in the state. Set when the machine is created and again whenever `password_version` changes.",
				Validators:          []validator.String{stringvalidator.RegexMatches(passwordRe, "must be 8-128 printable ASCII characters")},
			},
			"password_version": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Change this number to make the provider set `password` again (a write-only value has no diff of its own).",
				Validators:          []validator.Int64{int64validator.AlsoRequires(path.MatchRoot("password"))},
			},
			"node": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Owner only (the server does not implement it yet): node to create the machine on instead of letting boxbox3 choose. Used only at creation; where the machine is now is `current_node`.",
			},
			"user": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Login user of the machine (sudo without a password). Setting it is owner only (the server does not implement it yet); otherwise it is the tenant's name. Changing it replaces the machine.",
				Validators:          []validator.String{stringvalidator.RegexMatches(loginRe, "must be at most 32 characters of a-z 0-9 _ -, starting with a letter"), stringvalidator.NoneOf("root")},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplaceIfConfigured()},
			},
			"cloud_init": schema.StringAttribute{
				Optional: true, Sensitive: true, WriteOnly: true,
				MarkdownDescription: "Owner only (the server does not implement it yet): a `#cloud-config` document as content (at most 32768 bytes), merged with boxbox3's own. Write-only: used only at creation, never stored in the state.",
				Validators:          []validator.String{stringvalidator.LengthBetween(1, 32768)},
			},
			"iso": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Owner only (the server does not implement it yet): instead of `image`, an empty disk and an ISO of the `local` storage; the machine is not started. Changing it replaces the machine.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"vmid": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "PVE's ID of the machine; never changes.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"source": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`api`: made through the API. (`config` machines of the owner cannot be managed here.)",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"phase": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`creating`, `ready`, `failed` or `destroying`.",
			},
			"reason": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Why the phase is `failed`; otherwise null.",
			},
			"current_node": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Node the machine is on now. Can change (the owner's `migrate`, HA).",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`running` or `stopped`, as PVE says. The tenant starts and stops the machine in PVE, not here.",
			},
			"ha_nodes": schema.ListAttribute{
				ElementType:         types.StringType,
				Computed:            true,
				MarkdownDescription: "Nodes PVE replicates to now (can be fewer than asked while replicas are being made). Empty without `ha`.",
			},
			"ha_protected": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "`true` when every replica asked for has a successful sync and the machine is an HA resource.",
			},
			"addresses": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Addresses of the machine.",
				PlanModifiers:       []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
				Attributes: map[string]schema.Attribute{
					"nat":      schema.StringAttribute{Computed: true, MarkdownDescription: "IPv4 address in the NAT network (the way out)."},
					"internal": schema.StringAttribute{Computed: true, MarkdownDescription: "IPv4 address in the tenant's internal network."},
				},
			},
			"pending": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "What waits for the machine's next stop and start from PVE; null when nothing waits.",
				Attributes: map[string]schema.Attribute{
					"cpu_max": schema.SingleNestedAttribute{
						Computed: true,
						Attributes: map[string]schema.Attribute{
							"running":    schema.Int64Attribute{Computed: true},
							"configured": schema.Int64Attribute{Computed: true},
						},
					},
					"memory": schema.SingleNestedAttribute{
						Computed: true,
						Attributes: map[string]schema.Attribute{
							"running":    schema.StringAttribute{Computed: true},
							"configured": schema.StringAttribute{Computed: true},
						},
					},
				},
			},
			"timeouts": timeouts.Attributes(ctx, timeouts.Opts{
				Create: true, Update: true, Delete: true,
				CreateDescription: "How long to wait for a new machine to be `ready`. Default 45m (the server gives up on a first boot after 35 min).",
				UpdateDescription: "How long to wait when a change resumes an unfinished creation. Default 45m.",
				DeleteDescription: "How long to wait for the machine to be gone. Default 20m.",
			}),
		},
	}
}

type cpuMinValidator struct{}

func (cpuMinValidator) Description(context.Context) string {
	return "between 0 and 100 in steps of 0.05"
}
func (v cpuMinValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (v cpuMinValidator) ValidateFloat64(ctx context.Context, req validator.Float64Request, resp *validator.Float64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	f := req.ConfigValue.ValueFloat64()
	if f < 0 || f > 100 || math.Abs(f*20-math.Round(f*20)) > 1e-9 {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid guaranteed threads", "Must be "+v.Description(ctx)+".")
	}
}

func known(v attr.Value) bool { return !v.IsNull() && !v.IsUnknown() }

func (r *machineResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var m machineModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	d := &resp.Diagnostics
	switch {
	case m.Image.IsUnknown() || m.ISO.IsUnknown():
	case m.Image.IsNull() && m.ISO.IsNull():
		d.AddAttributeError(path.Root("image"), "Missing image", "Set `image` (or, as the owner, `iso`).")
	case !m.Image.IsNull() && !m.ISO.IsNull():
		d.AddAttributeError(path.Root("iso"), "Both image and iso", "A machine is made from an `image` or with an `iso`, not both.")
	}
	if known(m.ISO) {
		for name, v := range map[string]attr.Value{"ssh_keys": m.SSHKeys, "user": m.User, "cloud_init": m.CloudInit, "password": m.Password} {
			if !v.IsNull() {
				d.AddAttributeError(path.Root(name), "Not with iso", "`"+name+"` cannot be set with `iso`: the machine has no cloud-init and is not started.")
			}
		}
	} else if m.ISO.IsNull() && m.SSHKeys.IsNull() {
		d.AddAttributeError(path.Root("ssh_keys"), "Missing ssh_keys", "A machine made from an image needs at least one SSH public key: there is no other way in at first.")
	}
	if known(m.CPU) {
		var cpu cpuModel
		d.Append(m.CPU.As(ctx, &cpu, basetypes.ObjectAsOptions{})...)
		maxThreads := int64(2)
		if known(cpu.Max) {
			maxThreads = cpu.Max.ValueInt64()
		}
		if known(cpu.Min) && !cpu.Max.IsUnknown() && cpu.Min.ValueFloat64() > float64(maxThreads) {
			d.AddAttributeError(path.Root("cpu").AtName("min"), "Guaranteed threads above visible threads",
				fmt.Sprintf("`cpu.min` (%v) cannot be more than `cpu.max` (%d).", cpu.Min.ValueFloat64(), maxThreads))
		}
	}
	if known(m.HA) {
		var ha haModel
		d.Append(m.HA.As(ctx, &ha, basetypes.ObjectAsOptions{})...)
		switch {
		case ha.Replicas.IsUnknown() || ha.Nodes.IsUnknown():
		case ha.Replicas.IsNull() && ha.Nodes.IsNull():
			d.AddAttributeError(path.Root("ha").AtName("replicas"), "Missing replicas", "`ha` needs `replicas` (or, as the owner, `nodes`).")
		case !ha.Replicas.IsNull() && !ha.Nodes.IsNull():
			d.AddAttributeError(path.Root("ha").AtName("nodes"), "Both replicas and nodes", "Set `replicas` or `nodes`, not both.")
		}
		if known(ha.Nodes) && m.Node.IsNull() {
			d.AddAttributeError(path.Root("ha").AtName("nodes"), "Missing node", "`ha.nodes` needs `node`.")
		}
	}
}

func (r *machineResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan machineModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.Tenant.IsUnknown() && r.client != nil {
		var cfg types.String
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("tenant"), &cfg)...)
		if cfg.IsNull() {
			t := r.client.TokenTenant()
			if t == "" {
				resp.Diagnostics.AddAttributeError(path.Root("tenant"), "Missing tenant", "The provider's API token is the owner's, so there is no tenant to default to: set `tenant`.")
				return
			}
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("tenant"), types.StringValue(t))...)
			if known(plan.Name) {
				resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), types.StringValue(t+"/"+plan.Name.ValueString()))...)
			}
		}
	}
	if req.State.Raw.IsNull() {
		return
	}
	var state machineModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	d := &resp.Diagnostics
	keepStateIfSame(&resp.Plan, req.State, machineSettable, d)
	const refused = "The API will refuse this change. To do it anyway, replace the machine: `-replace=<address>` destroys it and creates a new one."
	if !state.SSHKeys.IsNull() && !plan.SSHKeys.IsUnknown() && !plan.SSHKeys.Equal(state.SSHKeys) {
		d.AddAttributeWarning(path.Root("ssh_keys"), "ssh_keys does not change an existing machine",
			"SSH keys are used only when the machine is created. The new value is recorded in the state, but the keys inside the machine stay as they are: change them inside the machine, or replace the machine.")
	}
	if !state.Node.IsNull() && !plan.Node.IsUnknown() && !plan.Node.Equal(state.Node) {
		d.AddAttributeWarning(path.Root("node"), "node does not move an existing machine",
			"`node` is used only when the machine is created. Moving a machine is the owner's `boxbox3 migrate`.")
	}
	if was, ok := state.Disk.Size(); ok {
		if now, ok := plan.Disk.Size(); ok && now.MiB() < was.MiB() {
			d.AddAttributeWarning(path.Root("disk"), "A disk never shrinks", fmt.Sprintf("The disk is %s and the configuration asks for %s. %s", was, now, refused))
		}
	}
	if known(state.HA) && !plan.HA.IsUnknown() {
		var was, now haModel
		d.Append(state.HA.As(ctx, &was, basetypes.ObjectAsOptions{})...)
		if !plan.HA.IsNull() {
			d.Append(plan.HA.As(ctx, &now, basetypes.ObjectAsOptions{})...)
		}
		if plan.HA.IsNull() || (known(now.Replicas) && known(was.Replicas) && now.Replicas.ValueInt64() < was.Replicas.ValueInt64()) {
			d.AddAttributeWarning(path.Root("ha"), "Replicas are never removed", "The configuration asks for fewer replicas than the machine has. "+refused)
		}
	}
}

// spec is the request body: the planned values, and from the configuration
// what the plan and the state never hold (write-only values) or hold as
// computed (the owner's fields, which a tenant must not send).
func spec(ctx context.Context, plan, cfg *machineModel, d *diag.Diagnostics) client.MachineSpec {
	out := client.MachineSpec{
		Image:            plan.Image.ValueString(),
		Node:             plan.Node.ValueString(),
		ISO:              plan.ISO.ValueString(),
		User:             cfg.User.ValueString(),
		TailscaleAuthKey: cfg.TailscaleAuthKey.ValueString(),
		CloudInit:        cfg.CloudInit.ValueString(),
	}
	var cpu cpuModel
	d.Append(plan.CPU.As(ctx, &cpu, basetypes.ObjectAsOptions{})...)
	out.CPU = &client.CPU{Min: math.Round(cpu.Min.ValueFloat64()*20) / 20, Max: cpu.Max.ValueInt64()}
	if s, ok := plan.Memory.Size(); ok {
		out.Memory = &s
	}
	if s, ok := plan.Disk.Size(); ok {
		out.Disk = &s
	}
	if !plan.SSHKeys.IsNull() {
		d.Append(plan.SSHKeys.ElementsAs(ctx, &out.SSHKeys, false)...)
	}
	if !plan.HA.IsNull() {
		var ha, cfgHA haModel
		d.Append(plan.HA.As(ctx, &ha, basetypes.ObjectAsOptions{})...)
		if known(cfg.HA) {
			d.Append(cfg.HA.As(ctx, &cfgHA, basetypes.ObjectAsOptions{})...)
		}
		out.HA = &client.HASpec{}
		if known(cfgHA.Nodes) {
			d.Append(cfgHA.Nodes.ElementsAs(ctx, &out.HA.Nodes, false)...)
		} else if known(ha.Replicas) {
			out.HA.Replicas = ha.Replicas.ValueInt64Pointer()
		}
		if known(cfgHA.Restart) {
			out.HA.Restart = cfgHA.Restart.ValueBoolPointer()
		}
	}
	return out
}

// refresh writes what the API says about the machine into the model. What
// the API never returns (ssh_keys, node, iso, write-only values) stays.
func (m *machineModel) refresh(ctx context.Context, api *client.Machine, d *diag.Diagnostics) {
	m.ID = types.StringValue(api.Tenant + "/" + api.Name)
	m.Tenant = types.StringValue(api.Tenant)
	m.Name = types.StringValue(api.Name)
	// An adopted machine has no known image: what the configuration says stays.
	if api.Image != nil || m.Image.IsUnknown() {
		m.Image = types.StringPointerValue(api.Image)
	}
	m.User = types.StringPointerValue(api.User)
	m.Memory = NewSizeValue(api.Memory)
	m.Disk = NewSizeValue(api.Disk)
	m.VMID = types.Int64Value(api.VMID)
	m.Source = types.StringValue(api.Source)
	m.Phase = types.StringValue(api.Phase)
	m.Reason = types.StringPointerValue(api.Reason)
	m.CurrentNode = types.StringValue(api.Node)
	m.Status = types.StringValue(api.Status)

	var diags diag.Diagnostics
	m.CPU, diags = types.ObjectValueFrom(ctx, cpuAttrTypes, cpuFrom(api.CPU))
	d.Append(diags...)
	m.Addresses, diags = types.ObjectValueFrom(ctx, addressesAttrTypes, addressesFrom(api.Addresses))
	d.Append(diags...)
	m.Pending, diags = types.ObjectValueFrom(ctx, pendingAttrTypes, pendingFrom(api.Pending))
	d.Append(diags...)

	if api.HA == nil {
		m.HA = types.ObjectNull(haAttrTypes)
		m.HANodes = stringList(ctx, nil, d)
		m.HAProtected = types.BoolValue(false)
		return
	}
	prior := haModel{Nodes: types.SetNull(types.StringType)}
	if known(m.HA) {
		d.Append(m.HA.As(ctx, &prior, basetypes.ObjectAsOptions{})...)
	}
	restart := types.BoolValue(true)
	switch {
	case api.HA.Restart != nil:
		restart = types.BoolValue(*api.HA.Restart)
	case known(prior.Restart):
		restart = prior.Restart
	}
	m.HA, diags = types.ObjectValueFrom(ctx, haAttrTypes, haModel{
		Replicas: types.Int64Value(api.HA.Replicas), Nodes: prior.Nodes, Restart: restart,
	})
	d.Append(diags...)
	m.HANodes = stringList(ctx, api.HA.Nodes, d)
	m.HAProtected = types.BoolValue(api.HA.Protected)
}

func pendingText(p *client.Pending) string {
	var parts []string
	if p.CPUMax != nil {
		parts = append(parts, fmt.Sprintf("cpu.max: the machine runs with %d, %d is configured", p.CPUMax.Running, p.CPUMax.Configured))
	}
	if p.Memory != nil {
		parts = append(parts, fmt.Sprintf("memory: the machine runs with %s, %s is configured", p.Memory.Running, p.Memory.Configured))
	}
	return strings.Join(parts, "; ")
}

func failedDetail(m *client.Machine) string {
	reason := "(no reason given)"
	if m.Reason != nil {
		reason = *m.Reason
	}
	return fmt.Sprintf("The server stopped working on %s/%s: %s\n\nThe machine still exists in PVE and uses quota.", m.Tenant, m.Name, reason)
}

// wait polls until the machine is ready. It always returns the last answer
// it saw (or `from`), so the state can be kept true even on failure.
func (r *machineResource) wait(ctx context.Context, from *client.Machine, timeout time.Duration, d *diag.Diagnostics) *client.Machine {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	m, err := r.client.WaitCreated(wctx, from.Tenant, from.Name)
	if m == nil {
		m = from
	}
	var we *client.WaitError
	switch {
	case errors.As(err, &we) && ctx.Err() == nil:
		d.AddError("Timed out waiting for the machine to be ready",
			fmt.Sprintf("%s/%s was still `%s` after %s. The server goes on with the work (it gives up on a first boot after 35 min). "+
				"The machine exists and is recorded in the state as tainted: the next apply replaces it. If it has become ready meanwhile, `untaint` it instead. "+
				"A longer wait is `timeouts.create`.", from.Tenant, from.Name, m.Phase, timeout))
	case err != nil:
		addAPIError(d, "Waiting for the machine failed", err)
	case m.Phase == client.PhaseFailed:
		d.AddError("The machine failed", failedDetail(m)+" It is recorded in the state as tainted: the next apply destroys it and creates it again.")
	}
	return m
}

func (r *machineResource) setPassword(ctx context.Context, m *client.Machine, password types.String, d *diag.Diagnostics) {
	if password.IsNull() {
		return
	}
	if err := r.client.SetPassword(ctx, m.Tenant, m.Name, password.ValueString()); err != nil {
		addAPIError(d, "Setting the console password failed", err)
	}
}

func (r *machineResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, cfg machineModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !known(plan.Tenant) {
		t := r.client.TokenTenant()
		if t == "" {
			resp.Diagnostics.AddAttributeError(path.Root("tenant"), "Missing tenant", "The provider's API token is the owner's: set `tenant`.")
			return
		}
		plan.Tenant = types.StringValue(t)
	}
	timeout, diags := plan.Timeouts.Create(ctx, defaultCreateTimeout)
	resp.Diagnostics.Append(diags...)
	body := spec(ctx, &plan, &cfg, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	tenant, name := plan.Tenant.ValueString(), plan.Name.ValueString()

	// `PUT` both creates and changes. Terraform's create must not quietly take
	// over, and change, a machine somebody else made.
	if _, err := r.client.GetMachine(ctx, tenant, name); err == nil {
		resp.Diagnostics.AddError("The machine already exists",
			fmt.Sprintf("%s/%s is already there. To manage it with this resource, import it: `import boxbox_machine.<name> %s/%s`.", tenant, name, tenant, name))
		return
	} else if !client.IsNotFound(err) {
		addAPIError(&resp.Diagnostics, "Reading the machine failed", err)
		return
	}
	m, _, err := r.client.PutMachine(ctx, tenant, name, body)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Creating the machine failed", err)
		return
	}
	// From here on the machine exists: whatever happens, it goes into the
	// state, so that a failure leaves it tainted and not forgotten.
	if m.Phase != client.PhaseReady {
		m = r.wait(ctx, m, timeout, &resp.Diagnostics)
	}
	if !resp.Diagnostics.HasError() {
		r.setPassword(ctx, m, cfg.Password, &resp.Diagnostics)
	}
	plan.refresh(ctx, m, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *machineResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state machineModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	m, err := r.client.GetMachine(ctx, state.Tenant.ValueString(), state.Name.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Reading the machine failed", err)
		return
	}
	if m.Phase == client.PhaseFailed {
		resp.Diagnostics.AddWarning("The machine is in phase failed", failedDetail(m)+" Replace it (`-replace=<address>`), or change something in its configuration: any update sends the request again, which makes the server retry.")
	}
	state.refresh(ctx, m, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *machineResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, cfg, state machineModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, diags := plan.Timeouts.Update(ctx, defaultUpdateTimeout)
	resp.Diagnostics.Append(diags...)
	body := spec(ctx, &plan, &cfg, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	m, _, err := r.client.PutMachine(ctx, plan.Tenant.ValueString(), plan.Name.ValueString(), body)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Changing the machine failed", err)
		return
	}
	// A `PUT` to a failed machine makes the server try again: wait for it.
	if m.Phase == client.PhaseCreating || m.Phase == client.PhaseFailed {
		m = r.wait(ctx, m, timeout, &resp.Diagnostics)
	}
	if !resp.Diagnostics.HasError() && !plan.PasswordVersion.Equal(state.PasswordVersion) {
		r.setPassword(ctx, m, cfg.Password, &resp.Diagnostics)
	}
	if m.Pending != nil {
		resp.Diagnostics.AddWarning("The change takes effect when the machine is next stopped and started",
			pendingText(m.Pending)+".\n\nThe API never restarts a machine. Stop and start it from PVE (the web interface or PVE's API); "+
				"a reboot from inside the machine is not enough. Until then the `pending` attribute shows what waits.")
	}
	failed := resp.Diagnostics.HasError()
	if failed {
		// Keep what the user asked for out of the state when the work did not
		// finish: only what the API reports is recorded.
		state.refresh(ctx, m, &resp.Diagnostics)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	plan.refresh(ctx, m, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *machineResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state machineModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, diags := state.Timeouts.Delete(ctx, defaultDeleteTimeout)
	resp.Diagnostics.Append(diags...)
	tenant, name := state.Tenant.ValueString(), state.Name.ValueString()
	if _, err := r.client.DeleteMachine(ctx, tenant, name); err != nil {
		if client.IsNotFound(err) {
			return
		}
		addAPIError(&resp.Diagnostics, "Destroying the machine failed", err)
		return
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := r.client.WaitGone(wctx, tenant, name)
	var we *client.WaitError
	switch {
	case errors.As(err, &we) && ctx.Err() == nil:
		resp.Diagnostics.AddError("Timed out waiting for the machine to be gone",
			fmt.Sprintf("%s/%s was still there after %s. The destruction goes on at the server (a replicated machine takes up to 10 min); run again to wait more. A longer wait is `timeouts.delete`.", tenant, name, timeout))
	case err != nil:
		addAPIError(&resp.Diagnostics, "Waiting for the machine to be gone failed", err)
	}
}

func (r *machineResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, adopt := strings.CutSuffix(req.ID, adoptSuffix)
	tenant, name, ok := strings.Cut(id, "/")
	if !ok || !client.ValidTenantName(tenant) || !client.ValidMachineName(name) {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("The import ID is `<tenant>/<name>`, like `demo/web1` (or, for the owner, `<tenant>/<name>%s` to adopt a machine made by the owner's config); got %q.", adoptSuffix, req.ID))
		return
	}
	m, err := r.client.GetMachine(ctx, tenant, name)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Reading the machine failed", err)
		return
	}
	if m.Source != client.SourceAPI {
		made := fmt.Sprintf("%s/%s is made by the owner's boxbox3 config (source %q): the API reads it but does not change or destroy it until the owner adopts it.", tenant, name, m.Source)
		switch {
		case r.client.TokenTenant() != "":
			resp.Diagnostics.AddError("The machine is not the API's to manage",
				made+" Adopting is the owner's: a tenant's token cannot do it. Ask the owner to adopt the machine, then import it as `"+id+"`; until then the `boxbox_machine` data source reads it.")
			return
		case !adopt:
			resp.Diagnostics.AddError("The machine is not the API's to manage",
				made+" To adopt it as part of the import, import it as `"+id+adoptSuffix+"`. Adopting changes the cluster: from then on the machine is the API's (and this resource's) to change and destroy, and the owner's config must no longer define it.")
			return
		}
		if m, err = r.client.AdoptMachine(ctx, tenant, name); err != nil {
			addAPIError(&resp.Diagnostics, "Adopting the machine failed", err)
			return
		}
		if m.Source != client.SourceAPI {
			resp.Diagnostics.AddError("Adopting the machine failed", fmt.Sprintf("The server answered the adoption of %s/%s with source %q.", tenant, name, m.Source))
			return
		}
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("tenant"), tenant)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
}
