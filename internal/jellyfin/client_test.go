package jellyfin

import "testing"

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"192.168.1.5":                 "http://192.168.1.5:8096",
		"nas.local:8920":              "http://nas.local:8920",
		"http://nas.local":            "http://nas.local",
		"https://jf.example.com/jf/":  "https://jf.example.com/jf",
		" http://10.0.0.2:8096/?x=1 ": "http://10.0.0.2:8096",
	}
	for in, want := range cases {
		got, err := NormalizeURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeURL(""); err == nil {
		t.Error("empty URL should fail")
	}
}

func TestAuthHeaderSanitizes(t *testing.T) {
	c := &Client{Device: `my "mac", home`, DeviceID: "abc", Token: "tok"}
	want := `MediaBrowser Client="Claude Jellyfin", Device="my mac  home", DeviceId="abc", Version="` + Version + `", Token="tok"`
	if got := c.AuthHeader(); got != want {
		t.Errorf("AuthHeader() = %s\nwant %s", got, want)
	}
}
