package fetch

import "testing"

func TestCleanLink(t *testing.T) {
	cases := map[string]string{
		"https://thenextweb.com/news/x?utm_source=tldrai":        "https://thenextweb.com/news/x",
		"https://example.com/a?id=7&utm_medium=email&ref=hn#top": "https://example.com/a?id=7",
		"https://news.ycombinator.com/item?id=123":               "https://news.ycombinator.com/item?id=123",
		"  urn:uuid:1234  ": "urn:uuid:1234",
	}
	for in, want := range cases {
		if got := CleanLink(in); got != want {
			t.Errorf("CleanLink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestURLHash_CollapsesVariants(t *testing.T) {
	variants := []string{
		"https://www.example.com/post/",
		"http://example.com/post?utm_source=tldr",
		"https://EXAMPLE.com/post#comments",
	}
	want := (&Article{Link: "https://example.com/post"}).URLHash()
	for _, v := range variants {
		if got := (&Article{Link: v}).URLHash(); got != want {
			t.Errorf("URLHash(%q) = %s, want %s", v, got, want)
		}
	}
	if other := (&Article{Link: "https://example.com/other"}).URLHash(); other == want {
		t.Error("different paths must not collide")
	}
}
