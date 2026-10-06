package util

import "testing"

func TestIsCustomEndpointPresent(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{name: "nil map", annotations: nil, want: false},
		{name: "unrelated annotation", annotations: map[string]string{"foo": "bar"}, want: false},
		{name: "empty value", annotations: map[string]string{CustomEndpointProvidedAnnotation: ""}, want: true},
		{name: "true", annotations: map[string]string{CustomEndpointProvidedAnnotation: "true"}, want: true},
		{name: "false still counts as present", annotations: map[string]string{CustomEndpointProvidedAnnotation: "false"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCustomEndpointPresent(tt.annotations); got != tt.want {
				t.Fatalf("IsCustomEndpointPresent(%v) = %v, want %v", tt.annotations, got, tt.want)
			}
		})
	}
}
