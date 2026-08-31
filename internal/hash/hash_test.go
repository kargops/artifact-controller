package hash

import (
	"strings"
	"testing"
)

func TestCanonicalIsOrderIndependentAndStable(t *testing.T) {
	a := map[string]string{"source": "client-repo.git", "gitRef": "v1.4.2", "platform": "windows", "arch": "x86"}
	b := map[string]string{"arch": "x86", "platform": "windows", "gitRef": "v1.4.2", "source": "client-repo.git"}

	ha, hb := Canonical(a), Canonical(b)
	if ha != hb {
		t.Fatalf("hash not order independent: %s != %s", ha, hb)
	}
	if !strings.HasPrefix(ha, "sha256:") || len(ha) != len("sha256:")+64 {
		t.Fatalf("unexpected hash format: %s", ha)
	}

	// Golden value: this must never change across releases (it is the
	// store-key/provenance contract).
	const golden = "sha256:72ddd979f2af866f8356b3ffe6e43584f7233bef60021f9882d40cc26f8bc776"
	if ha != golden {
		t.Fatalf("canonical hash contract broken: got %s, frozen golden %s", ha, golden)
	}
}

func TestCanonicalDiffersOnValueChange(t *testing.T) {
	a := map[string]string{"k": "v1"}
	b := map[string]string{"k": "v2"}
	if Canonical(a) == Canonical(b) {
		t.Fatal("different identities must hash differently")
	}
}

func TestShort(t *testing.T) {
	h := Canonical(map[string]string{"k": "v"})
	s := Short(h, 8)
	if len(s) != 8 || strings.Contains(s, ":") {
		t.Fatalf("short hash malformed: %q", s)
	}
}
