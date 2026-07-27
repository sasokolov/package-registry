package provider

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/sasokolov/package-registry/terraform-provider-registry/internal/client"
)

// Reading a resource back has one rule: report what the registry says, but do
// not turn "the operator never set this" into "the operator set the zero
// value". The registry omits defaults, so a naive copy would make every plan
// show null -> "" or null -> false forever. Each helper below therefore takes
// the prior state and keeps it when the two mean the same thing — and only
// then. Real drift still shows up as drift.

func stringOrPrior(v string, prior types.String) types.String {
	if v == "" && prior.IsNull() {
		return types.StringNull()
	}
	return types.StringValue(v)
}

func boolOrPrior(v bool, prior types.Bool) types.Bool {
	if !v && prior.IsNull() {
		return types.BoolNull()
	}
	return types.BoolValue(v)
}

func float64OrPrior(v float64, prior types.Float64) types.Float64 {
	if v == 0 && prior.IsNull() {
		return types.Float64Null()
	}
	return types.Float64Value(v)
}

// durationOrPrior keeps the operator's spelling when it means the same
// duration. The registry normalises "20m" to "20m0s"; both are the same
// twenty minutes, and a plan that says so every time teaches people to
// ignore plans.
func durationOrPrior(v string, prior types.String) types.String {
	if v == "" {
		if prior.IsNull() {
			return types.StringNull()
		}
		// The server dropped it: that is a real change.
		return types.StringValue("")
	}
	if !prior.IsNull() && sameDuration(prior.ValueString(), v) {
		return prior
	}
	return types.StringValue(v)
}

func sameDuration(a, b string) bool {
	da, err := time.ParseDuration(a)
	if err != nil {
		return false
	}
	db, err := time.ParseDuration(b)
	if err != nil {
		return false
	}
	return da == db
}

// jsonOrPrior keeps the operator's rendering of a JSON value when it is the
// same value. jsonencode({...}) and the registry's own encoder agree on
// content but not always on spacing or number formatting.
func jsonOrPrior(v map[string]any, prior types.String) (types.String, error) {
	if len(v) == 0 {
		if prior.IsNull() {
			return types.StringNull(), nil
		}
		return types.StringValue(""), nil
	}
	if !prior.IsNull() && sameJSON(prior.ValueString(), v) {
		return prior, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return types.StringNull(), err
	}
	return types.StringValue(string(raw)), nil
}

func sameJSON(raw string, v map[string]any) bool {
	var lhs any
	if err := json.Unmarshal([]byte(raw), &lhs); err != nil {
		return false
	}
	// Compare through a JSON round trip on both sides so numbers, which are
	// float64 either way, are compared as the same kind of thing.
	normalised, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var rhs any
	if err := json.Unmarshal(normalised, &rhs); err != nil {
		return false
	}
	return reflect.DeepEqual(lhs, rhs)
}

// stringsOrPrior is stringOrPrior for lists of strings.
func stringsOrPrior(ctx context.Context, v []string, prior types.List) (types.List, diag.Diagnostics) {
	if len(v) == 0 && prior.IsNull() {
		return types.ListNull(types.StringType), nil
	}
	return types.ListValueFrom(ctx, types.StringType, v)
}

// stringsFrom reads a list attribute into a slice; null and empty both mean
// "nothing configured".
func stringsFrom(ctx context.Context, list types.List, diags *diag.Diagnostics) []string {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	var out []string
	diags.Append(list.ElementsAs(ctx, &out, false)...)
	return out
}

// isNotFound reports whether an error means the resource is already gone,
// which makes a delete a success rather than a failure.
func isNotFound(err error) bool { return errors.Is(err, client.ErrNotFound) }
