package maintenance

import "testing"

func TestIsVMClusterTag(t *testing.T) {
	tests := []struct {
		tag  string
		want bool
	}{
		{tag: "maas-lxd-wlc-abc", want: true},
		{tag: "maas-lxd-wlc-1234", want: true},
		{tag: TagVMControlPlane, want: false},
		{tag: TagVMClusterPrefix + "worker", want: false},
		{tag: "maas-lxd-ready-op-abc", want: false},
		{tag: "", want: false},
	}
	for _, tt := range tests {
		if got := IsVMClusterTag(tt.tag); got != tt.want {
			t.Errorf("IsVMClusterTag(%q) = %v, want %v", tt.tag, got, tt.want)
		}
	}
}
