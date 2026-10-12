package platform

import "testing"

func TestParseLsofPicksTheLoopbackListener(t *testing.T) {
	out := "p401\ncpostgres\nn192.168.1.20:24190\np517\ncnode\nn127.0.0.1:24190\n"
	pid, name, ok := parseLsof(out, 24190)
	if !ok || pid != 517 || name != "node" {
		t.Fatalf("got %d %q %v, want 517 node", pid, name, ok)
	}
	if _, _, ok := parseLsof("p401\ncpostgres\nn192.168.1.20:24190\n", 24190); ok {
		t.Fatal("a LAN listener on the same port number is not the conflict")
	}
	if pid, _, ok := parseLsof("p9\ncpython3\nn*:24190\n", 24190); !ok || pid != 9 {
		t.Fatal("a wildcard listener conflicts")
	}
	if pid, _, ok := parseLsof("p9\ncpython3\nn[::1]:24190\n", 24190); !ok || pid != 9 {
		t.Fatal("an IPv6 loopback listener conflicts")
	}
	if _, _, ok := parseLsof("p9\ncpython3\nn127.0.0.1:24191\n", 24190); ok {
		t.Fatal("another port is not the conflict")
	}
}

func TestParseSSPicksTheLoopbackListener(t *testing.T) {
	out := "LISTEN 0 4096 192.168.1.20:24190 0.0.0.0:* users:((\"postgres\",pid=401,fd=5))\n" +
		"LISTEN 0 511 [::1]:24190 [::]:* users:((\"node\",pid=517,fd=18))\n"
	pid, name, ok := parseSS(out, 24190)
	if !ok || pid != 517 || name != "node" {
		t.Fatalf("got %d %q %v, want 517 node", pid, name, ok)
	}
	if _, _, ok := parseSS("LISTEN 0 4096 192.168.1.20:24190 0.0.0.0:* users:((\"x\",pid=1,fd=1))\n", 24190); ok {
		t.Fatal("LAN only must not match")
	}
}

func TestParseNetTCPPicksTheLoopbackListener(t *testing.T) {
	out := "401\tpostgres\t192.168.1.20\r\n517\tnode\t127.0.0.1\r\n"
	pid, name, ok := parseNetTCP(out)
	if !ok || pid != 517 || name != "node" {
		t.Fatalf("got %d %q %v, want 517 node", pid, name, ok)
	}
	if pid, _, ok := parseNetTCP("8\tpython\t::\r\n"); !ok || pid != 8 {
		t.Fatal("the IPv6 wildcard conflicts")
	}
}
