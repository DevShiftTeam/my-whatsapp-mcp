//go:build !native_onnx

package inference

func NewNativeEncoder(NativeConfig) (Backend, error)       { return nil, ErrUnavailable }
func NewLegacyNativeEncoder(NativeConfig) (Backend, error) { return nil, ErrUnavailable }
