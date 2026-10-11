package main

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRemovedCLIFlagsExitWithMantlePointer(t *testing.T) {
	cases := []struct {
		args        []string
		flag        string
		replacement string
	}{
		{[]string{"--cli"}, "cli", "blocknet attach"},
		{[]string{"--daemon=false"}, "daemon", "blocknet attach"},
		{[]string{"--recover"}, "recover", "blocknet attach, then: import (option 1, recovery seed)"},
		{[]string{"--viewonly"}, "viewonly", "blocknet attach, then: import (option 3, view-only)"},
		{[]string{"--spend-pub", strings.Repeat("11", 32)}, "spend-pub", "blocknet attach, then: import (option 3, view-only)"},
		{[]string{"--view-priv", strings.Repeat("22", 32)}, "view-priv", "blocknet attach, then: import (option 3, view-only)"},
		{[]string{"--view-priv-env", "CUSTOM_VIEW_PRIV"}, "view-priv-env", "blocknet attach, then: import (option 3, view-only)"},
		{[]string{"--output-peer-address"}, "output-peer-address", "blocknet attach, then: export-peer"},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			args := append([]string{"--data", t.TempDir(), "--version"}, tc.args...)
			res := runMainHelper(t, helperRun{args: args})
			if res.exitCode != 1 {
				t.Fatalf("exit %d, want 1; stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
			}
			if res.stdout != "" {
				t.Fatalf("expected no stdout, got %q", res.stdout)
			}
			for _, want := range []string{
				"The Blocknet core command-line interface has been removed.",
				"https://github.com/blocknetprivacy/blocknet/releases/latest",
				"Replacement for --" + tc.flag + ": " + tc.replacement,
			} {
				if !strings.Contains(res.stderr, want) {
					t.Fatalf("stderr missing %q: %q", want, res.stderr)
				}
			}
		})
	}
}

func TestDaemonFlagsStillAccepted(t *testing.T) {
	res := runMainHelper(t, helperRun{args: []string{
		"--daemon", "--testnet", "--api", "127.0.0.1:0", "--data", t.TempDir(), "--wallet", "w.dat",
		"--listen", "/ip4/127.0.0.1/tcp/0", "--seed", "--explorer", "127.0.0.1:0", "--full-sync",
		"--save-checkpoints", "--p2p-max-inbound", "4", "--p2p-max-outbound", "4",
		"--p2p-whitelist", "", "--no-version-check", "--nocolor", "--version",
	}})
	if res.exitCode != 0 || strings.TrimSpace(res.stdout) != Version {
		t.Fatalf("exit %d stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
	}
}

// ---- Helper subprocess harness ----

type helperRun struct {
	args  []string
	env   map[string]string // value "" means unset
	stdin string
}

type helperResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func runMainHelper(t *testing.T, run helperRun) helperResult {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=TestHelperProcessMain", "--"}, run.args...)...)
	cmd.Env = applyEnv(os.Environ(), run.env)
	cmd.Stdin = strings.NewReader(run.stdin)
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("failed to run helper: %v", err)
		}
	}
	return helperResult{
		exitCode: exitCode,
		stdout:   out.String(),
		stderr:   errBuf.String(),
	}
}

func applyEnv(base []string, updates map[string]string) []string {
	// Convert to map for easy update/unset.
	m := make(map[string]string, len(base))
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	m["GO_WANT_HELPER_PROCESS"] = "1"
	for k, v := range updates {
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func TestHelperProcessMain(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	// Extract args after "--".
	args := []string{}
	for i := 0; i < len(os.Args); i++ {
		if os.Args[i] == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	os.Args = append([]string{os.Args[0]}, args...)

	// Reset global flags so main() can parse in this subprocess.
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	main()
	os.Exit(0)
}
