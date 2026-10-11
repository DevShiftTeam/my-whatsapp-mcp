package compute

import "errors"

// ResidencyPolicy names the two candidates for measured process-tree residency.
// A passing policy is selected in the release profile, never inferred from an
// idle/instantaneous sample. CPU admission alone does not prove loaded RSS.
type ResidencyPolicy string

const (
	EncoderResident ResidencyPolicy = "encoder_resident"
	SerialModels    ResidencyPolicy = "serial_models"
)

var ErrResidencyPolicy = errors.New("compute_residency_policy_invalid")

func (p ResidencyPolicy) Validate() error {
	if p != EncoderResident && p != SerialModels {
		return ErrResidencyPolicy
	}
	return nil
}
