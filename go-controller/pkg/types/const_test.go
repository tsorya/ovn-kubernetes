package types

import "testing"

func TestGetPatchPortSuffix(t *testing.T) {
	tests := []struct {
		name       string
		bridgeName string
		want       string
	}{
		{
			name:       "default bridge name",
			bridgeName: "br-int",
			want:       "-to-br-int",
		},
		{
			name:       "custom bridge name",
			bridgeName: "br-custom",
			want:       "-to-br-custom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetPatchPortSuffix(tt.bridgeName)
			if got != tt.want {
				t.Errorf("GetPatchPortSuffix(%q) = %q, want %q", tt.bridgeName, got, tt.want)
			}
		})
	}
}
