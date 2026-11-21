package provider

import (
	"context"
	"errors"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/triac-hq/terraform-provider-boxbox/internal/client"
)

func asAPIError(err error, target **client.APIError) bool { return errors.As(err, target) }

var (
	tenantNameRe  = regexp.MustCompile(`^[a-z][a-z0-9]{1,7}$`)
	machineNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

const (
	tenantNameMsg  = "must be 2-8 characters of a-z and 0-9, starting with a letter"
	machineNameMsg = "must be 1-63 characters of a-z, 0-9 and -, starting and ending with a letter or a digit"
)

// Models shared by resources and data sources. A struct is an object; a
// pointer to a struct is an object that may be null.

type cpuModel struct {
	Min types.Float64 `tfsdk:"min"`
	Max types.Int64   `tfsdk:"max"`
}

func cpuFrom(c client.CPU) cpuModel {
	return cpuModel{Min: types.Float64Value(c.Min), Max: types.Int64Value(c.Max)}
}

var cpuAttrTypes = map[string]attr.Type{"min": types.Float64Type, "max": types.Int64Type}

type addressesModel struct {
	NAT      types.String `tfsdk:"nat"`
	Internal types.String `tfsdk:"internal"`
}

var addressesAttrTypes = map[string]attr.Type{"nat": types.StringType, "internal": types.StringType}

type pendingIntModel struct {
	Running    types.Int64 `tfsdk:"running"`
	Configured types.Int64 `tfsdk:"configured"`
}

type pendingSizeModel struct {
	Running    types.String `tfsdk:"running"`
	Configured types.String `tfsdk:"configured"`
}

type pendingModel struct {
	CPUMax *pendingIntModel  `tfsdk:"cpu_max"`
	Memory *pendingSizeModel `tfsdk:"memory"`
}

var pendingAttrTypes = map[string]attr.Type{
	"cpu_max": types.ObjectType{AttrTypes: map[string]attr.Type{"running": types.Int64Type, "configured": types.Int64Type}},
	"memory":  types.ObjectType{AttrTypes: map[string]attr.Type{"running": types.StringType, "configured": types.StringType}},
}

func addressesFrom(a client.Addresses) addressesModel {
	return addressesModel{NAT: types.StringPointerValue(a.NAT), Internal: types.StringPointerValue(a.Internal)}
}

func pendingFrom(p *client.Pending) *pendingModel {
	if p == nil {
		return nil
	}
	var out pendingModel
	if p.CPUMax != nil {
		out.CPUMax = &pendingIntModel{Running: types.Int64Value(p.CPUMax.Running), Configured: types.Int64Value(p.CPUMax.Configured)}
	}
	if p.Memory != nil {
		out.Memory = &pendingSizeModel{Running: types.StringValue(p.Memory.Running.String()), Configured: types.StringValue(p.Memory.Configured.String())}
	}
	return &out
}

type quotaModel struct {
	Machines types.Int64 `tfsdk:"machines"`
	CPU      cpuModel    `tfsdk:"cpu"`
	Memory   SizeValue   `tfsdk:"memory"`
	Disk     SizeValue   `tfsdk:"disk"`
	HA       types.Bool  `tfsdk:"ha"`
}

func quotaFrom(q *client.Quota) *quotaModel {
	if q == nil {
		return nil
	}
	return &quotaModel{
		Machines: types.Int64Value(q.Machines), CPU: cpuFrom(q.CPU),
		Memory: NewSizeValue(q.Memory), Disk: NewSizeValue(q.Disk), HA: types.BoolValue(q.HA),
	}
}

type usageModel struct {
	Machines types.Int64  `tfsdk:"machines"`
	CPU      cpuModel     `tfsdk:"cpu"`
	Memory   types.String `tfsdk:"memory"`
	Disk     types.String `tfsdk:"disk"`
}

var usageAttrTypes = map[string]attr.Type{
	"machines": types.Int64Type,
	"cpu":      types.ObjectType{AttrTypes: cpuAttrTypes},
	"memory":   types.StringType,
	"disk":     types.StringType,
}

func usageFrom(u client.Usage) usageModel {
	return usageModel{
		Machines: types.Int64Value(u.Machines), CPU: cpuFrom(u.CPU),
		Memory: types.StringValue(u.Memory.String()), Disk: types.StringValue(u.Disk.String()),
	}
}

// keepStateIfSame makes the plan the state when none of the attributes a
// configuration can set differs between them. The framework marks every
// computed attribute unknown as soon as the configuration's text differs from
// the state's, before plan modifiers have judged the difference (a size in
// another unit, say); without this that would be an update of nothing.
func keepStateIfSame(plan *tfsdk.Plan, state tfsdk.State, settable []string, d *diag.Diagnostics) {
	var was, now map[string]tftypes.Value
	if err := state.Raw.As(&was); err != nil {
		d.AddError("Reading the state failed", err.Error())
		return
	}
	if err := plan.Raw.As(&now); err != nil {
		d.AddError("Reading the plan failed", err.Error())
		return
	}
	for _, name := range settable {
		if !now[name].IsFullyKnown() || !now[name].Equal(was[name]) {
			return
		}
	}
	plan.Raw = state.Raw
}

func stringList(ctx context.Context, in []string, diags *diag.Diagnostics) types.List {
	if in == nil {
		in = []string{}
	}
	v, d := types.ListValueFrom(ctx, types.StringType, in)
	diags.Append(d...)
	return v
}
