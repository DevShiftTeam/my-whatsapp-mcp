package compute

import (
	"errors"
	"testing"
)

// An unspecified/unrestricted policy must never silently permit extra models.
func TestResidencyPolicyRequiresExplicitSupportedSelection(t *testing.T) {
	for _, policy := range []ResidencyPolicy{EncoderResident, SerialModels} {
		if err := policy.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, policy := range []ResidencyPolicy{"", "unrestricted", "serial", "encoder_resident_unchecked"} {
		if err := policy.Validate(); !errors.Is(err, ErrResidencyPolicy) {
			t.Fatalf("accepted policy %q: %v", policy, err)
		}
	}
}
