package identity

import "testing"

func TestSanitizeHostname(t *testing.T) {
	cases := map[string]string{
		"MOHSIN":          "mohsin",
		"DESKTOP-ABC123":  "desktop-abc123",
		"  Server 2019  ": "server-2019",
		"PC_1":            "pc-1",
		"a.b.c":           "a-b-c",
		"héllo":           "h-llo",
		"---":             "unknown-device",
		"":                "unknown-device",
		"UPPER lower 123": "upper-lower-123",
	}
	for in, want := range cases {
		if got := SanitizeHostname(in); got != want {
			t.Errorf("SanitizeHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeHostnameCapsLength(t *testing.T) {
	long := ""
	for i := 0; i < 100; i++ {
		long += "a"
	}
	got := SanitizeHostname(long)
	if len(got) > 63 {
		t.Errorf("hostname not capped: got %d characters", len(got))
	}
}

func TestSanitizeHostnameIsIdempotent(t *testing.T) {
	once := SanitizeHostname("  Mohsin PC_1  ")
	twice := SanitizeHostname(once)
	if once != twice {
		t.Errorf("not idempotent: %q then %q", once, twice)
	}
}

func TestCollectIsPopulated(t *testing.T) {
	info := Collect()
	if info.Hostname == "" {
		t.Error("hostname is empty")
	}
	if info.OSCaption == "" {
		t.Error("os caption is empty")
	}
	if info.Arch == "" {
		t.Error("arch is empty")
	}
	if got := EnrollHostname(); got == "" {
		t.Error("EnrollHostname returned empty")
	}
}
