package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/triac-hq/terraform-provider-boxbox/internal/client"
)

// ---- boxbox_me ----

func newMeDataSource() datasource.DataSource { return &meDataSource{} }

type meDataSource struct{ client *client.Client }

type meQuotaModel struct {
	Machines types.Int64  `tfsdk:"machines"`
	CPU      cpuModel     `tfsdk:"cpu"`
	Memory   types.String `tfsdk:"memory"`
	Disk     types.String `tfsdk:"disk"`
	HA       types.Bool   `tfsdk:"ha"`
}

type meModel struct {
	User   types.String  `tfsdk:"user"`
	Role   types.String  `tfsdk:"role"`
	Tenant types.String  `tfsdk:"tenant"`
	Quota  *meQuotaModel `tfsdk:"quota"`
	Usage  *usageModel   `tfsdk:"usage"`
	Images types.List    `tfsdk:"images"`
}

func cpuSchema(minDoc, maxDoc string) schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Computed: true,
		Attributes: map[string]schema.Attribute{
			"min": schema.Float64Attribute{Computed: true, MarkdownDescription: minDoc},
			"max": schema.Int64Attribute{Computed: true, MarkdownDescription: maxDoc},
		},
	}
}

func (d *meDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_me"
}

func (d *meDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (d *meDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Who the provider's API token is, and the tenant's quota and usage (`GET me`).",
		Attributes: map[string]schema.Attribute{
			"user":   schema.StringAttribute{Computed: true, MarkdownDescription: "PVE user of the token, `<name>@pve`."},
			"role":   schema.StringAttribute{Computed: true, MarkdownDescription: "`tenant` or `admin`."},
			"tenant": schema.StringAttribute{Computed: true, MarkdownDescription: "Name of the tenant; null for the owner."},
			"quota": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The tenant's quota; null when the tenant has none (then it cannot create machines) and for the owner.",
				Attributes: map[string]schema.Attribute{
					"machines": schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of machines."},
					"cpu":      cpuSchema("Guaranteed threads in total.", "Visible threads in total."),
					"memory":   schema.StringAttribute{Computed: true, MarkdownDescription: "Memory in total."},
					"disk":     schema.StringAttribute{Computed: true, MarkdownDescription: "Disks in total, as asked for."},
					"ha":       schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the tenant may ask for `ha.replicas`."},
				},
			},
			"usage": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "What all machines in the tenant's pool add up to, whoever made them; null for the owner.",
				Attributes: map[string]schema.Attribute{
					"machines": schema.Int64Attribute{Computed: true},
					"cpu":      cpuSchema("Guaranteed threads in use.", "Visible threads in use."),
					"memory":   schema.StringAttribute{Computed: true},
					"disk":     schema.StringAttribute{Computed: true},
				},
			},
			"images": schema.ListAttribute{ElementType: types.StringType, Computed: true, MarkdownDescription: "Images a machine can be created from."},
		},
	}
}

func (d *meDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	me, err := d.client.Me(ctx)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Reading `me` failed", err)
		return
	}
	m := meModel{
		User: types.StringValue(me.User), Role: types.StringValue(me.Role),
		Tenant: types.StringNull(), Images: stringList(ctx, me.Images, &resp.Diagnostics),
	}
	if t := me.Tenant; t != nil {
		m.Tenant = types.StringValue(t.Name)
		u := usageFrom(t.Usage)
		m.Usage = &u
		if q := t.Quota; q != nil {
			m.Quota = &meQuotaModel{
				Machines: types.Int64Value(q.Machines), CPU: cpuFrom(q.CPU),
				Memory: types.StringValue(q.Memory.String()), Disk: types.StringValue(q.Disk.String()), HA: types.BoolValue(q.HA),
			}
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// ---- boxbox_machine and boxbox_machines ----

type dsHAModel struct {
	Replicas  types.Int64 `tfsdk:"replicas"`
	Nodes     types.List  `tfsdk:"nodes"`
	Protected types.Bool  `tfsdk:"protected"`
	Restart   types.Bool  `tfsdk:"restart"`
}

// dsMachineModel is a machine in the API's own shape.
type dsMachineModel struct {
	ID        types.String   `tfsdk:"id"`
	Tenant    types.String   `tfsdk:"tenant"`
	Name      types.String   `tfsdk:"name"`
	VMID      types.Int64    `tfsdk:"vmid"`
	Source    types.String   `tfsdk:"source"`
	Phase     types.String   `tfsdk:"phase"`
	Reason    types.String   `tfsdk:"reason"`
	Node      types.String   `tfsdk:"node"`
	Status    types.String   `tfsdk:"status"`
	Image     types.String   `tfsdk:"image"`
	User      types.String   `tfsdk:"user"`
	CPU       cpuModel       `tfsdk:"cpu"`
	Memory    types.String   `tfsdk:"memory"`
	Disk      types.String   `tfsdk:"disk"`
	HA        *dsHAModel     `tfsdk:"ha"`
	Addresses addressesModel `tfsdk:"addresses"`
	Pending   *pendingModel  `tfsdk:"pending"`
}

func dsMachineFrom(ctx context.Context, m *client.Machine, d *diag.Diagnostics) dsMachineModel {
	out := dsMachineModel{
		ID:     types.StringValue(m.Tenant + "/" + m.Name),
		Tenant: types.StringValue(m.Tenant), Name: types.StringValue(m.Name),
		VMID: types.Int64Value(m.VMID), Source: types.StringValue(m.Source),
		Phase: types.StringValue(m.Phase), Reason: types.StringPointerValue(m.Reason),
		Node: types.StringValue(m.Node), Status: types.StringValue(m.Status),
		Image: types.StringPointerValue(m.Image), User: types.StringPointerValue(m.User),
		CPU: cpuFrom(m.CPU), Memory: types.StringValue(m.Memory.String()), Disk: types.StringValue(m.Disk.String()),
		Addresses: addressesFrom(m.Addresses), Pending: pendingFrom(m.Pending),
	}
	if m.HA != nil {
		out.HA = &dsHAModel{
			Replicas: types.Int64Value(m.HA.Replicas), Nodes: stringList(ctx, m.HA.Nodes, d),
			Protected: types.BoolValue(m.HA.Protected), Restart: types.BoolPointerValue(m.HA.Restart),
		}
	}
	return out
}

// machineAttributes is the schema of a machine in the API's shape. In the
// single data source `tenant` and `name` are the arguments.
func machineAttributes(single bool) map[string]schema.Attribute {
	tenant := schema.StringAttribute{Computed: true, MarkdownDescription: "Tenant that owns the machine."}
	name := schema.StringAttribute{Computed: true, MarkdownDescription: "Name of the machine."}
	if single {
		tenant = schema.StringAttribute{
			Optional: true, Computed: true,
			MarkdownDescription: "Tenant that owns the machine. Defaults to the tenant of the provider's API token.",
			Validators:          []validator.String{stringvalidator.RegexMatches(tenantNameRe, tenantNameMsg)},
		}
		name = schema.StringAttribute{
			Required: true, MarkdownDescription: "Name of the machine.",
			Validators: []validator.String{stringvalidator.RegexMatches(machineNameRe, machineNameMsg)},
		}
	}
	pair := func(a schema.Attribute) schema.SingleNestedAttribute {
		return schema.SingleNestedAttribute{Computed: true, Attributes: map[string]schema.Attribute{"running": a, "configured": a}}
	}
	return map[string]schema.Attribute{
		"id":     schema.StringAttribute{Computed: true, MarkdownDescription: "`<tenant>/<name>`."},
		"tenant": tenant,
		"name":   name,
		"vmid":   schema.Int64Attribute{Computed: true, MarkdownDescription: "PVE's ID of the machine."},
		"source": schema.StringAttribute{Computed: true, MarkdownDescription: "`api` (made through the API) or `config` (made by the owner's config; read-only here)."},
		"phase":  schema.StringAttribute{Computed: true, MarkdownDescription: "`creating`, `ready`, `failed` or `destroying`."},
		"reason": schema.StringAttribute{Computed: true, MarkdownDescription: "Why the phase is `failed`; otherwise null."},
		"node":   schema.StringAttribute{Computed: true, MarkdownDescription: "Node the machine is on now."},
		"status": schema.StringAttribute{Computed: true, MarkdownDescription: "`running` or `stopped`."},
		"image":  schema.StringAttribute{Computed: true, MarkdownDescription: "Image the machine was created from; null when not known (`config` machine)."},
		"user":   schema.StringAttribute{Computed: true, MarkdownDescription: "Login user; null for a `config` machine."},
		"cpu":    cpuSchema("Guaranteed threads.", "Threads the machine sees (as configured; see `pending`)."),
		"memory": schema.StringAttribute{Computed: true, MarkdownDescription: "Memory as configured (see `pending`)."},
		"disk":   schema.StringAttribute{Computed: true, MarkdownDescription: "Disk."},
		"ha": schema.SingleNestedAttribute{
			Computed: true, MarkdownDescription: "Replication and HA; null without replicas.",
			Attributes: map[string]schema.Attribute{
				"replicas":  schema.Int64Attribute{Computed: true, MarkdownDescription: "Replicas asked for."},
				"nodes":     schema.ListAttribute{ElementType: types.StringType, Computed: true, MarkdownDescription: "Nodes PVE replicates to now."},
				"protected": schema.BoolAttribute{Computed: true, MarkdownDescription: "Every replica has a successful sync and the machine is an HA resource."},
				"restart":   schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the machine is an HA resource; null until the server reports it."},
			},
		},
		"addresses": schema.SingleNestedAttribute{
			Computed: true,
			Attributes: map[string]schema.Attribute{
				"nat":      schema.StringAttribute{Computed: true, MarkdownDescription: "IPv4 address in the NAT network."},
				"internal": schema.StringAttribute{Computed: true, MarkdownDescription: "IPv4 address in the tenant's internal network."},
			},
		},
		"pending": schema.SingleNestedAttribute{
			Computed: true, MarkdownDescription: "What waits for the next stop and start from PVE; null when nothing waits.",
			Attributes: map[string]schema.Attribute{
				"cpu_max": pair(schema.Int64Attribute{Computed: true}),
				"memory":  pair(schema.StringAttribute{Computed: true}),
			},
		},
	}
}

func newMachineDataSource() datasource.DataSource { return &machineDataSource{} }

type machineDataSource struct{ client *client.Client }

func (d *machineDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_machine"
}

func (d *machineDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (d *machineDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One machine, also one made by the owner's config (`GET machines/{tenant}/{name}`).",
		Attributes:          machineAttributes(true),
	}
}

func (d *machineDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var tenant, name types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("tenant"), &tenant)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("name"), &name)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t := tenant.ValueString()
	if t == "" {
		if t = d.client.TokenTenant(); t == "" {
			resp.Diagnostics.AddAttributeError(path.Root("tenant"), "Missing tenant", "The provider's API token is the owner's, so there is no tenant to default to: set `tenant`.")
			return
		}
	}
	m, err := d.client.GetMachine(ctx, t, name.ValueString())
	if err != nil {
		addAPIError(&resp.Diagnostics, "Reading the machine failed", err)
		return
	}
	out := dsMachineFrom(ctx, m, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}

func newMachinesDataSource() datasource.DataSource { return &machinesDataSource{} }

type machinesDataSource struct{ client *client.Client }

type machinesModel struct {
	Tenant   types.String     `tfsdk:"tenant"`
	Machines []dsMachineModel `tfsdk:"machines"`
}

func (d *machinesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_machines"
}

func (d *machinesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (d *machinesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The caller's machines: a tenant's own pool, for the owner every tenant's (`GET machines`). Machines made by the owner's config are included.",
		Attributes: map[string]schema.Attribute{
			"tenant": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Only this tenant's machines. A tenant can name only itself.",
				Validators:          []validator.String{stringvalidator.RegexMatches(tenantNameRe, tenantNameMsg)},
			},
			"machines": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The machines, ordered by tenant, then name.",
				NestedObject:        schema.NestedAttributeObject{Attributes: machineAttributes(false)},
			},
		},
	}
}

func (d *machinesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m machinesModel
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("tenant"), &m.Tenant)...)
	if resp.Diagnostics.HasError() {
		return
	}
	list, err := d.client.ListMachines(ctx, m.Tenant.ValueString())
	if err != nil {
		addAPIError(&resp.Diagnostics, "Listing the machines failed", err)
		return
	}
	m.Machines = []dsMachineModel{}
	for i := range list {
		m.Machines = append(m.Machines, dsMachineFrom(ctx, &list[i], &resp.Diagnostics))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
