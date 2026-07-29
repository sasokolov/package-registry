package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// These decide whether a plan is honest. Every helper here answers the same
// question in a different shape: is what the registry reports the same thing
// the operator wrote? Getting it wrong in one direction hides real drift; in
// the other it invents a change on every refresh until people stop reading
// plans.

func TestUnsetOptionalsStayUnset(t *testing.T) {
	if got := stringOrPrior("", types.StringNull()); !got.IsNull() {
		t.Errorf("an absent string became %v", got)
	}
	if got := boolOrPrior(false, types.BoolNull()); !got.IsNull() {
		t.Errorf("an absent bool became %v", got)
	}
	if got := float64OrPrior(0, types.Float64Null()); !got.IsNull() {
		t.Errorf("an absent number became %v", got)
	}
}

func TestAnExplicitZeroIsKept(t *testing.T) {
	// The registry omits `anonymous: false` because it is the default. An
	// operator who wrote it still wrote it, and flipping it to null would be
	// a change on every single refresh.
	if got := boolOrPrior(false, types.BoolValue(false)); got.IsNull() || got.ValueBool() {
		t.Errorf("an explicit false became %v", got)
	}
	if got := float64OrPrior(0, types.Float64Value(0)); got.IsNull() {
		t.Errorf("an explicit 0 became null")
	}
	if got := stringOrPrior("", types.StringValue("")); got.IsNull() {
		t.Errorf("an explicit empty string became null")
	}
}

func TestRealDriftIsNotSwallowed(t *testing.T) {
	if got := boolOrPrior(false, types.BoolValue(true)); got.IsNull() || got.ValueBool() {
		t.Errorf("true changed to false was reported as %v", got)
	}
	if got := stringOrPrior("lazy", types.StringValue("eager")); got.ValueString() != "lazy" {
		t.Errorf("a changed string was reported as %v", got)
	}
	if got := durationOrPrior("1h0m0s", types.StringValue("20m")); got.ValueString() != "1h0m0s" {
		t.Errorf("a changed duration was reported as %v", got)
	}
}

func TestDurationsThatMeanTheSameThingAreTheSameThing(t *testing.T) {
	tests := []struct {
		server, prior, want string
	}{
		{"20m0s", "20m", "20m"},
		{"1h0m0s", "60m", "60m"},
		{"90s", "1m30s", "1m30s"},
		// Not a duration on either side: report what the server says.
		{"20m0s", "twenty minutes", "20m0s"},
	}
	for _, tc := range tests {
		got := durationOrPrior(tc.server, types.StringValue(tc.prior))
		if got.ValueString() != tc.want {
			t.Errorf("durationOrPrior(%q, %q) = %q, want %q",
				tc.server, tc.prior, got.ValueString(), tc.want)
		}
	}
	if got := durationOrPrior("", types.StringNull()); !got.IsNull() {
		t.Errorf("an absent duration became %v", got)
	}
}

func TestPolicyOptionsKeepTheOperatorsRendering(t *testing.T) {
	// jsonencode puts no spaces in; a human might. Both are the same options.
	prior := types.StringValue(`{"allow": ["com.example:liba"]}`)
	got, err := jsonOrPrior(map[string]any{"allow": []any{"com.example:liba"}}, prior)
	if err != nil {
		t.Fatalf("jsonOrPrior: %v", err)
	}
	if got.ValueString() != prior.ValueString() {
		t.Errorf("the operator's rendering was rewritten to %q", got.ValueString())
	}

	// Different options are different.
	got, err = jsonOrPrior(map[string]any{"allow": []any{"com.example:libb"}}, prior)
	if err != nil {
		t.Fatalf("jsonOrPrior: %v", err)
	}
	if got.ValueString() == prior.ValueString() {
		t.Error("changed options were reported as unchanged")
	}

	// An explicitly empty object stays explicitly empty.
	empty := types.StringValue("{}")
	got, err = jsonOrPrior(nil, empty)
	if err != nil {
		t.Fatalf("jsonOrPrior: %v", err)
	}
	if got.ValueString() != "{}" {
		t.Errorf("jsonencode({}) became %q", got.ValueString())
	}

	// No options at all stays absent.
	got, err = jsonOrPrior(nil, types.StringNull())
	if err != nil {
		t.Fatalf("jsonOrPrior: %v", err)
	}
	if !got.IsNull() {
		t.Errorf("absent options became %v", got)
	}
}

// An empty list is the trap: converting a nil slice produces a *null* list,
// so `publishers = []` would flip to null on every refresh and never settle.
func TestAnExplicitlyEmptyListStaysEmptyNotNull(t *testing.T) {
	ctx := context.Background()

	priorEmpty, diags := types.ListValueFrom(ctx, types.StringType, []string{})
	if diags.HasError() {
		t.Fatalf("build empty list: %v", diags)
	}
	got, diags := stringsOrPrior(ctx, nil, priorEmpty)
	if diags.HasError() {
		t.Fatalf("stringsOrPrior: %v", diags)
	}
	if got.IsNull() {
		t.Fatal("an explicitly empty list became null")
	}
	if len(got.Elements()) != 0 {
		t.Fatalf("an empty list gained %d elements", len(got.Elements()))
	}

	// Never set stays never set.
	got, diags = stringsOrPrior(ctx, nil, types.ListNull(types.StringType))
	if diags.HasError() {
		t.Fatalf("stringsOrPrior: %v", diags)
	}
	if !got.IsNull() {
		t.Fatal("an absent list became empty")
	}

	// And a list the registry actually has is reported.
	got, diags = stringsOrPrior(ctx, []string{"token:ci-*"}, types.ListNull(types.StringType))
	if diags.HasError() {
		t.Fatalf("stringsOrPrior: %v", diags)
	}
	if len(got.Elements()) != 1 {
		t.Fatalf("publishers came back as %v", got)
	}
}
