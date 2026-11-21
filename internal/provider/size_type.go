package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/triac-hq/terraform-provider-boxbox/internal/client"
)

// sameSize keeps the state's text when the configuration says the same
// amount in another form (`4096MiB` and `4GiB`). The framework applies a
// type's semantic equality to what a provider returns, not to the plan, so
// without this a change of form alone would be a diff.
type sameSize struct{}

func (sameSize) Description(context.Context) string {
	return "The same amount written in another unit is not a change."
}

func (m sameSize) MarkdownDescription(ctx context.Context) string { return m.Description(ctx) }

func (sameSize) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.StateValue.IsNull() || req.PlanValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	was, errWas := client.ParseSize(req.StateValue.ValueString())
	now, errNow := client.ParseSize(req.PlanValue.ValueString())
	if errWas == nil && errNow == nil && was == now {
		resp.PlanValue = req.StateValue
	}
}

// SizeType is a string holding a size (`4GiB`, `512MiB`). Two sizes are the
// same value when they are the same amount: the API answers in its canonical
// form (`4GiB`), and a configuration that says `4096MiB` must not be a diff.
type SizeType struct{ basetypes.StringType }

var (
	_ basetypes.StringTypable                    = SizeType{}
	_ basetypes.StringValuableWithSemanticEquals = SizeValue{}
)

// Equal tells whether the other type is a SizeType.
func (t SizeType) Equal(o attr.Type) bool {
	_, ok := o.(SizeType)
	return ok
}

func (t SizeType) String() string { return "SizeType" }

// ValueFromString wraps a string.
func (t SizeType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return SizeValue{StringValue: in}, nil
}

// ValueFromTerraform wraps what Terraform sent.
func (t SizeType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	v, err := t.StringType.ValueFromTerraform(ctx, in)
	if err != nil {
		return nil, err
	}
	s, ok := v.(basetypes.StringValue)
	if !ok {
		return nil, fmt.Errorf("unexpected value type %T", v)
	}
	return SizeValue{StringValue: s}, nil
}

// ValueType is the value type of SizeType.
func (t SizeType) ValueType(context.Context) attr.Value { return SizeValue{} }

// SizeValue is a value of SizeType.
type SizeValue struct{ basetypes.StringValue }

// NewSizeValue is the API's size as a value.
func NewSizeValue(s client.Size) SizeValue {
	return SizeValue{StringValue: basetypes.NewStringValue(s.String())}
}

// Type is SizeType.
func (v SizeValue) Type(context.Context) attr.Type { return SizeType{} }

// Equal is exact equality, as the framework requires.
func (v SizeValue) Equal(o attr.Value) bool {
	other, ok := o.(SizeValue)
	return ok && v.StringValue.Equal(other.StringValue)
}

// StringSemanticEquals tells whether the two are the same amount.
func (v SizeValue) StringSemanticEquals(_ context.Context, other basetypes.StringValuable) (bool, diag.Diagnostics) {
	o, ok := other.(SizeValue)
	if !ok {
		return false, nil
	}
	a, errA := client.ParseSize(v.ValueString())
	b, errB := client.ParseSize(o.ValueString())
	if errA != nil || errB != nil {
		return v.ValueString() == o.ValueString(), nil
	}
	return a == b, nil
}

// Size parses the value; ok is false when it is null, unknown or not a size.
func (v SizeValue) Size() (client.Size, bool) {
	if v.IsNull() || v.IsUnknown() {
		return client.Size{}, false
	}
	s, err := client.ParseSize(v.ValueString())
	return s, err == nil
}

// sizeValidator checks a size's form and, for disks, the contract's limits.
type sizeValidator struct {
	wholeGiB bool
	minMiB   int64
}

func (v sizeValidator) Description(context.Context) string {
	d := "an integer and MiB, GiB or TiB without a space, like \"4GiB\""
	if v.wholeGiB {
		d += fmt.Sprintf("; whole gibibytes, at least %s", client.SizeFromMiB(v.minMiB))
	}
	return d
}

func (v sizeValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v sizeValidator) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	s, err := client.ParseSize(req.ConfigValue.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Not a size", err.Error())
		return
	}
	if v.wholeGiB && (!s.WholeGiB() || s.MiB() < v.minMiB) {
		resp.Diagnostics.AddAttributeError(req.Path, "Size out of range", "Must be "+v.Description(ctx)+".")
	}
}
