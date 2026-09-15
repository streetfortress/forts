package manifest

import "testing"

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int{"80G": 80 << 30, "512M": 512 << 20, " 2g ": 2 << 30} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "80", "0G", "-1G", "80GB", "x"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) should fail", in)
		}
	}
}
