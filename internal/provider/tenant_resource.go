package provider

import (
	"context"
	"fmt"
	"math"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/triac-hq/terraform-provider-boxbox/internal/client"
)

var (
	_ resource.Resource                = (*tenantResource)(nil)
	_ resource.ResourceWithConfigure   = (*tenantResource)(nil)
	_ resource.ResourceWithImportState = (*tenantResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*tenantResource)(nil)
)

func newTenantResource() resource.Resource { return &tenantResource{} }

type tenantResource struct{ client *client.Client }

type tenantModel struct {
	ID      types.String `tfsdk:"id"`
	Name    types.String `tfsdk:"name"`
	Email   types.String `tfsdk:"email"`
	Quota   *quotaModel  `tfsdk:"quota"`
	Usage   types.Object `tfsdk:"usage"`
	Network types.String `tfsdk:"network"`
}

func (r *tenantResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tenant"
}

func (r *tenantResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *tenantResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A tenant: its pool, its user `<name>@pve`, its internal network and its quota. Owner only.\n\n" +
			"**The server does not implement this yet** (it answers `501`): the resource follows the shape the API contract specifies.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The tenant's name; also the import ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Name of the tenant: 2-8 characters of a-z and 0-9, starting with a letter, not `admin`. Changing it replaces the tenant.",
				Validators:          []validator.String{stringvalidator.RegexMatches(tenantNameRe, tenantNameMsg), stringvalidator.NoneOf("admin")},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"email": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Address Cloudflare Access lets the tenant in with.",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"quota": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "What the tenant's machines may add up to. Without it the tenant cannot create machines itself. Lowering it below the usage is allowed: new requests are refused, existing machines are not touched.",
				Attributes: map[string]schema.Attribute{
					"machines": schema.Int64Attribute{
						Required: true, MarkdownDescription: "Number of machines.",
						Validators: []validator.Int64{int64validator.AtLeast(0)},
					},
					"cpu": schema.SingleNestedAttribute{
						Required: true,
						Attributes: map[string]schema.Attribute{
							"min": schema.Float64Attribute{
								Required: true, MarkdownDescription: "Guaranteed threads in total, in steps of 0.05.",
							},
							"max": schema.Int64Attribute{
								Required: true, MarkdownDescription: "Visible threads in total.",
								Validators: []validator.Int64{int64validator.AtLeast(0)},
							},
						},
					},
					"memory": schema.StringAttribute{
						CustomType: SizeType{}, Required: true, MarkdownDescription: "Memory in total (`\"8GiB\"`).",
						Validators:    []validator.String{sizeValidator{}},
						PlanModifiers: []planmodifier.String{sameSize{}},
					},
					"disk": schema.StringAttribute{
						CustomType: SizeType{}, Required: true, MarkdownDescription: "Disks in total, as asked for (replicas are not counted again).",
						Validators:    []validator.String{sizeValidator{}},
						PlanModifiers: []planmodifier.String{sameSize{}},
					},
					"ha": schema.BoolAttribute{
						Optional: true, Computed: true, Default: booldefault.StaticBool(false),
						MarkdownDescription: "Whether the tenant may ask for `ha.replicas`. Default `false`.",
					},
				},
			},
			"usage": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "What all machines in the tenant's pool add up to now.",
				Attributes: map[string]schema.Attribute{
					"machines": schema.Int64Attribute{Computed: true},
					"cpu": schema.SingleNestedAttribute{
						Computed: true,
						Attributes: map[string]schema.Attribute{
							"min": schema.Float64Attribute{Computed: true},
							"max": schema.Int64Attribute{Computed: true},
						},
					},
					"memory": schema.StringAttribute{Computed: true},
					"disk":   schema.StringAttribute{Computed: true},
				},
			},
			"network": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The tenant's internal network (CIDR), as PVE has it.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *tenantResource) ModifyPlan(_ context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}
	keepStateIfSame(&resp.Plan, req.State, []string{"name", "email", "quota"}, &resp.Diagnostics)
}

func (m *tenantModel) spec() client.TenantSpec {
	out := client.TenantSpec{Email: m.Email.ValueStringPointer()}
	if q := m.Quota; q != nil {
		memory, _ := q.Memory.Size()
		disk, _ := q.Disk.Size()
		out.Quota = &client.Quota{
			Machines: q.Machines.ValueInt64(),
			CPU:      client.CPU{Min: math.Round(q.CPU.Min.ValueFloat64()*20) / 20, Max: q.CPU.Max.ValueInt64()},
			Memory:   memory, Disk: disk, HA: q.HA.ValueBool(),
		}
	}
	return out
}

func (m *tenantModel) refresh(ctx context.Context, api *client.Tenant, d *diag.Diagnostics) {
	m.ID = types.StringValue(api.Name)
	m.Name = types.StringValue(api.Name)
	m.Email = types.StringPointerValue(api.Email)
	m.Quota = quotaFrom(api.Quota)
	m.Network = types.StringValue(api.Network)
	var diags diag.Diagnostics
	m.Usage, diags = types.ObjectValueFrom(ctx, usageAttrTypes, usageFrom(api.Usage))
	d.Append(diags...)
}

func (r *tenantResource) put(ctx context.Context, plan *tenantModel, summary string, d *diag.Diagnostics) {
	t, err := r.client.PutTenant(ctx, plan.Name.ValueString(), plan.spec())
	if err != nil {
		addAPIError(d, summary, err)
		return
	}
	plan.refresh(ctx, t, d)
}

func (r *tenantResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan tenantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()
	// As with machines: `PUT` also changes, and create must not take over.
	if _, err := r.client.GetTenant(ctx, name); err == nil {
		resp.Diagnostics.AddError("The tenant already exists",
			fmt.Sprintf("Tenant %s is already there. To manage it with this resource, import it: `import boxbox_tenant.<name> %s`.", name, name))
		return
	} else if !client.IsNotFound(err) {
		addAPIError(&resp.Diagnostics, "Reading the tenant failed", err)
		return
	}
	r.put(ctx, &plan, "Creating the tenant failed", &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *tenantResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state tenantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.client.GetTenant(ctx, state.Name.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Reading the tenant failed", err)
		return
	}
	state.refresh(ctx, t, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *tenantResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan tenantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.put(ctx, &plan, "Changing the tenant failed", &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *tenantResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state tenantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteTenant(ctx, state.Name.ValueString()); err != nil && !client.IsNotFound(err) {
		addAPIError(&resp.Diagnostics, "Deleting the tenant failed", err)
	}
}

func (r *tenantResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if !client.ValidTenantName(req.ID) {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("The import ID is the tenant's name, like `demo`; got %q.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
}
