package function

import "testing"

func TestValidateKVMSupported(t *testing.T) {
	tests := []struct {
		name        string
		machineType string
		wantErr     bool
	}{
		{"n2d allowed", "n2d-standard-4", false},
		{"n2 allowed", "n2-standard-2", false},
		{"n1 allowed", "n1-standard-4", false},
		{"c3 allowed", "c3-standard-8", false},
		{"c3d allowed", "c3d-standard-8", false},
		{"c2 allowed", "c2-standard-4", false},
		{"e2 rejected", "e2-standard-4", true},
		{"e2-micro rejected", "e2-micro", true},
		{"t2d rejected", "t2d-standard-2", true},
		{"t2a rejected", "t2a-standard-2", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKVMSupported(tt.machineType)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateKVMSupported(%q) err = %v, wantErr = %v", tt.machineType, err, tt.wantErr)
			}
		})
	}
}
