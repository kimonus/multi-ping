package ping

import "testing"

func TestResolveLiterals(t *testing.T) {
	for _, c := range []struct {
		host    string
		prefer6 bool
		want    string
		len     int
	}{
		{"8.8.8.8", false, "8.8.8.8", 4},
		{"8.8.8.8", true, "8.8.8.8", 4}, // the preference cannot change a literal
		{"2001:db8::1", false, "2001:db8::1", 16},
		{"fe80::1%eth0", false, "fe80::1", 16},
	} {
		ip, err := Resolve(c.host, c.prefer6)
		if err != nil || ip.String() != c.want || len(ip) != c.len {
			t.Errorf("Resolve(%q, %v) = %v (len %d), %v", c.host, c.prefer6, ip, len(ip), err)
		}
	}
}
