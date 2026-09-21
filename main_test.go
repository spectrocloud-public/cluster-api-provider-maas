/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"sort"
	"testing"

	"github.com/spf13/pflag"
	"sigs.k8s.io/cluster-api/util/flags"
)

// newTestFlagSet returns a fresh FlagSet with the manager's flags registered.
// It never touches pflag.CommandLine, which `go test` shares with flag.CommandLine,
// and it resets the package-level globals that initFlags rebinds.
func newTestFlagSet(t *testing.T) *pflag.FlagSet {
	t.Helper()
	metricsBindAddr = ""
	managerOptions = flags.ManagerOptions{}
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	initFlags(fs)
	return fs
}

// U1 — the diagnostics flag surface and its contracted defaults.
func TestInitFlags_DiagnosticsFlagDefaults(t *testing.T) {
	fs := newTestFlagSet(t)

	for _, tc := range []struct{ name, want string }{
		{"diagnostics-address", ":8443"},
		{"insecure-diagnostics", "false"},
		{"tls-min-version", "VersionTLS12"},
		{"tls-cipher-suites", "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fs.Lookup(tc.name)
			if f == nil {
				t.Fatalf("flag --%s is not registered", tc.name)
			}
			if f.DefValue != tc.want {
				t.Errorf("--%s default = %q, want %q", tc.name, f.DefValue, tc.want)
			}
		})
	}
}

// U2 — --metrics-bind-addr is registered as deprecated with an empty default.
func TestInitFlags_MetricsBindAddrDeprecated(t *testing.T) {
	fs := newTestFlagSet(t)

	f := fs.Lookup("metrics-bind-addr")
	if f == nil {
		t.Fatal("flag --metrics-bind-addr is not registered")
	}
	if f.Deprecated == "" {
		t.Error("--metrics-bind-addr is not marked deprecated")
	}
	if f.DefValue != "" {
		t.Errorf("--metrics-bind-addr default = %q, want %q", f.DefValue, "")
	}
}

// U3/U4 — the deprecated flag is still accepted, and does not alias the diagnostics address.
func TestInitFlags_MetricsBindAddrAcceptedAndIgnored(t *testing.T) {
	fs := newTestFlagSet(t)
	fs.SetOutput(discard{})

	if err := fs.Parse([]string{"--metrics-bind-addr=127.0.0.1:8080"}); err != nil {
		t.Fatalf("parsing the deprecated flag must not fail: %v", err)
	}
	if metricsBindAddr != "127.0.0.1:8080" {
		t.Errorf("metricsBindAddr = %q, want %q", metricsBindAddr, "127.0.0.1:8080")
	}
	if managerOptions.DiagnosticsAddress != ":8443" {
		t.Errorf("DiagnosticsAddress = %q, want %q (the deprecated flag must not alias it)",
			managerOptions.DiagnosticsAddress, ":8443")
	}

	_, metricsOpts, err := flags.GetManagerOptions(managerOptions)
	if err != nil {
		t.Fatalf("GetManagerOptions: %v", err)
	}
	if metricsOpts.BindAddress != ":8443" {
		t.Errorf("metrics BindAddress = %q, want %q", metricsOpts.BindAddress, ":8443")
	}
}

// U5 — secure mode: TLS, an authn/authz filter, and exactly the pprof + flags handlers.
func TestGetManagerOptions_SecureDefaults(t *testing.T) {
	fs := newTestFlagSet(t)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	_, metricsOpts, err := flags.GetManagerOptions(managerOptions)
	if err != nil {
		t.Fatalf("GetManagerOptions: %v", err)
	}
	if !metricsOpts.SecureServing {
		t.Error("SecureServing = false, want true by default")
	}
	if metricsOpts.FilterProvider == nil {
		t.Error("FilterProvider is nil; requests would not be authorised")
	}

	want := []string{
		"/debug/flags/v",
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/profile",
		"/debug/pprof/symbol",
		"/debug/pprof/trace",
	}
	got := make([]string, 0, len(metricsOpts.ExtraHandlers))
	for k, h := range metricsOpts.ExtraHandlers {
		if h == nil {
			t.Errorf("ExtraHandlers[%q] is nil", k)
		}
		got = append(got, k)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("ExtraHandlers keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ExtraHandlers keys = %v, want %v", got, want)
		}
	}
}

// U6 — insecure mode is strictly less capable: no TLS, no filter, no pprof.
func TestGetManagerOptions_InsecureDiagnostics(t *testing.T) {
	fs := newTestFlagSet(t)
	if err := fs.Parse([]string{"--insecure-diagnostics"}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	_, metricsOpts, err := flags.GetManagerOptions(managerOptions)
	if err != nil {
		t.Fatalf("GetManagerOptions: %v", err)
	}
	if metricsOpts.SecureServing {
		t.Error("SecureServing = true, want false with --insecure-diagnostics")
	}
	if metricsOpts.FilterProvider != nil {
		t.Error("FilterProvider is set in insecure mode")
	}
	if metricsOpts.ExtraHandlers != nil {
		t.Errorf("ExtraHandlers = %v, want nil (pprof must not be exposed unauthenticated)",
			metricsOpts.ExtraHandlers)
	}
}

// U7 — tlsOpts are produced and carry the minimum TLS version, shared with the webhook server.
func TestGetManagerOptions_TLSOpts(t *testing.T) {
	fs := newTestFlagSet(t)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}

	tlsOpts, _, err := flags.GetManagerOptions(managerOptions)
	if err != nil {
		t.Fatalf("GetManagerOptions: %v", err)
	}
	if len(tlsOpts) < 1 {
		t.Fatalf("len(tlsOpts) = %d, want >= 1", len(tlsOpts))
	}

	cfg := &tls.Config{} //nolint:gosec // MinVersion is what the options under test set.
	for _, apply := range tlsOpts {
		apply(cfg)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want %#x (VersionTLS12)", cfg.MinVersion, tls.VersionTLS12)
	}
}

// U8 — the pre-existing flags keep their names and defaults.
func TestInitFlags_ExistingFlagsUnchanged(t *testing.T) {
	fs := newTestFlagSet(t)

	for _, tc := range []struct{ name, want string }{
		{"machine-concurrency", "2"},
		{"leader-elect", "false"},
		{"sync-period", "2h0m0s"},
		{"health-addr", ":9440"},
		{"webhook-port", "0"},
		{"namespace", ""},
		{"cluster-role", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fs.Lookup(tc.name)
			if f == nil {
				t.Fatalf("flag --%s is not registered", tc.name)
			}
			if f.DefValue != tc.want {
				t.Errorf("--%s default = %q, want %q", tc.name, f.DefValue, tc.want)
			}
		})
	}
}

// U9 — an invalid TLS version surfaces an error, so main's os.Exit(1) branch is reachable.
func TestGetManagerOptions_InvalidTLSVersion(t *testing.T) {
	opts := flags.ManagerOptions{
		TLSMinVersion:      "VersionTLS99",
		DiagnosticsAddress: ":8443",
	}
	if _, _, err := flags.GetManagerOptions(opts); err == nil {
		t.Error("GetManagerOptions returned nil error for an invalid --tls-min-version")
	}
}

// discard swallows pflag's deprecation notice so the test output stays readable.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
