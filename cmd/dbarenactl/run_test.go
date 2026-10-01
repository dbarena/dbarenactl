package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfirmLowIterations(t *testing.T) {
	cases := []struct {
		name       string
		iterations int
		input      string
		want       bool
		wantPrompt bool
		wantErr    bool
	}{
		{name: "minimum skips prompt", iterations: 3, want: true},
		{name: "above minimum skips prompt", iterations: 6, want: true},
		{name: "below minimum, yes", iterations: 2, input: "y\n", want: true, wantPrompt: true},
		{name: "below minimum, no", iterations: 2, input: "n\n", want: false, wantPrompt: true},
		{name: "below minimum, empty answer", iterations: 1, input: "\n", want: false, wantPrompt: true},
		{name: "below minimum, no input", iterations: 1, input: "", wantPrompt: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := confirmLowIterations(strings.NewReader(tc.input), &out, tc.iterations)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			prompted := strings.Contains(out.String(), "requires at least 3 iterations. Proceed anyway?")
			if prompted != tc.wantPrompt {
				t.Errorf("prompted = %v, want %v (output %q)", prompted, tc.wantPrompt, out.String())
			}
		})
	}
}
