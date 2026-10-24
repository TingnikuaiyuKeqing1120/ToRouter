package app

import "testing"

func TestChannelConsoleURLKeepsExactLocalTarget(t *testing.T) {
	got, err := channelConsoleURL("http://127.0.0.1:19019/", 7, "cred&other=#two")
	if err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:19019/#/channels?channel_id=7&credential_id=cred%26other%3D%23two"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := channelConsoleURL("http://[::1]:19019", 7, "primary"); err != nil {
		t.Fatal(err)
	}
}

func TestChannelConsoleURLRejectsNonLocalOrAmbiguousTargets(t *testing.T) {
	for _, origin := range []string{
		"https://example.com:443", "http://example.com:19019", "http://127.0.0.1.evil.test:19019",
		"http://user@127.0.0.1:19019", "http://127.0.0.1:19019/path", "http://127.0.0.1:19019/?next=elsewhere",
		"http://127.0.0.1:19019/#elsewhere", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536",
	} {
		if _, err := channelConsoleURL(origin, 7, "primary"); err == nil {
			t.Errorf("accepted %q", origin)
		}
	}
	if _, err := channelConsoleURL("http://127.0.0.1:19019", 0, "primary"); err == nil {
		t.Fatal("accepted an empty channel target")
	}
	if _, err := channelConsoleURL("http://127.0.0.1:19019", 7, " "); err == nil {
		t.Fatal("accepted an empty credential target")
	}
}
