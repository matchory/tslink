package netutil

import (
	"os"
	"path/filepath"
	"testing"
)

// A chain and the jump to it are written in one transaction: the chain
// emptied and filled, the old jump deleted and the new one inserted first.
func TestChainWriteScript(t *testing.T) {
	w := chainWrite{
		table: "mangle", chain: "C", setRules: true,
		rules: [][]string{{"-d", "10.0.0.0/8", "-j", "DROP"}},
		from:  "FORWARD", jumpExists: true,
	}
	got, err := w.script()
	if err != nil {
		t.Fatal(err)
	}
	want := "*mangle\n:C - [0:0]\n-F C\n-A C -d 10.0.0.0/8 -j DROP\n" +
		"-D FORWARD -j C\n-I FORWARD 1 -j C\nCOMMIT\n"
	if got != want {
		t.Errorf("script:\n%s\nwant:\n%s", got, want)
	}

	// Only the jump, and a jump that does not exist yet is not deleted
	got, err = chainWrite{table: "mangle", chain: "C", from: "INPUT"}.script()
	if err != nil {
		t.Fatal(err)
	}
	if want := "*mangle\n-I INPUT 1 -j C\nCOMMIT\n"; got != want {
		t.Errorf("jump only:\n%s\nwant:\n%s", got, want)
	}
}

// An argument iptables-restore would split or read as a comment is refused,
// not written as another rule.
func TestChainWriteScriptRejectsUnsafeArguments(t *testing.T) {
	for _, arg := range []string{"", "a b", "a\n-F FORWARD", "#x", `"x"`} {
		w := chainWrite{table: "mangle", chain: "C", setRules: true, rules: [][]string{{"-i", arg}}}
		if _, err := w.script(); err == nil {
			t.Errorf("argument %q accepted", arg)
		}
	}
}

// The restore binary writes to the backend iptables uses.
func TestRestoreFor(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"iptables-nft-restore", "iptables-legacy-restore"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o700); err != nil { // #nosec G306
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	tests := map[string]string{
		"iptables v1.8.10 (nf_tables)": filepath.Join(dir, "iptables-nft-restore"),
		"iptables v1.8.10 (legacy)":    filepath.Join(dir, "iptables-legacy-restore"),
		"iptables v1.6.1":              "iptables-restore",
	}
	for version, want := range tests {
		if got := restoreFor("iptables", version); got != want {
			t.Errorf("%s: %s, want %s", version, got, want)
		}
	}
	// Not installed: the plain one
	if got := restoreFor("ip6tables", "ip6tables v1.8.10 (nf_tables)"); got != "ip6tables-restore" {
		t.Errorf("without ip6tables-nft-restore: %s", got)
	}
}
