package pressbooks

import "testing"

func TestParseBookRefAcceptsEveryDocumentedForm(t *testing.T) {
	const want = "https://ecampusontario.pressbooks.pub/northern/"
	for _, in := range []string{
		"https://ecampusontario.pressbooks.pub/northern/",
		"https://ecampusontario.pressbooks.pub/northern",
		"ecampusontario.pressbooks.pub/northern",
		"https://ecampusontario.pressbooks.pub/northern/chapter/chapter-1/",
		"https://ecampusontario.pressbooks.pub/northern/front-matter/introduction/",
		"https://ecampusontario.pressbooks.pub/northern/wp-json/pressbooks/v2/toc",
	} {
		got, err := ParseBookRef(in)
		if err != nil {
			t.Fatalf("ParseBookRef(%q): %v", in, err)
		}
		if got.URL != want {
			t.Errorf("ParseBookRef(%q).URL = %q, want %q", in, got.URL, want)
		}
		if got.Host != "ecampusontario.pressbooks.pub" || got.Slug != "northern" {
			t.Errorf("ParseBookRef(%q) = %#v", in, got)
		}
	}
}

// A single-book install serves the book at the host root, so the first path
// segment is a Pressbooks section name rather than a book slug. Reading it as a
// slug would send every request to a book that does not exist.
func TestParseBookRefHandlesRootHostedBooks(t *testing.T) {
	for _, in := range []string{
		"https://milnepublishing.geneseo.edu/",
		"milnepublishing.geneseo.edu",
		"https://milnepublishing.geneseo.edu/chapter/chapter-1/",
		"https://milnepublishing.geneseo.edu/back-matter/glossary/",
	} {
		got, err := ParseBookRef(in)
		if err != nil {
			t.Fatalf("ParseBookRef(%q): %v", in, err)
		}
		if got.Slug != "" {
			t.Errorf("ParseBookRef(%q).Slug = %q, want empty", in, got.Slug)
		}
		if got.URL != "https://milnepublishing.geneseo.edu/" {
			t.Errorf("ParseBookRef(%q).URL = %q", in, got.URL)
		}
	}
}

// httptest servers are always addressed as host:port with an explicit
// scheme. Losing the port would force every later task that tests against
// one to patch BookRef.URL back together by hand.
func TestParseBookRefPreservesPortAndScheme(t *testing.T) {
	got, err := ParseBookRef("http://127.0.0.1:8080/northern")
	if err != nil {
		t.Fatalf("ParseBookRef: %v", err)
	}
	if got.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want %q", got.Host, "127.0.0.1")
	}
	if got.Slug != "northern" {
		t.Errorf("Slug = %q, want %q", got.Slug, "northern")
	}
	if got.URL != "http://127.0.0.1:8080/northern/" {
		t.Errorf("URL = %q, want %q", got.URL, "http://127.0.0.1:8080/northern/")
	}

	root, err := ParseBookRef("http://127.0.0.1:8080/")
	if err != nil {
		t.Fatalf("ParseBookRef: %v", err)
	}
	if root.URL != "http://127.0.0.1:8080/" {
		t.Errorf("URL = %q, want %q", root.URL, "http://127.0.0.1:8080/")
	}

	// No port and no explicit scheme: unaffected, still defaults to https.
	plain, err := ParseBookRef("ecampusontario.pressbooks.pub/northern")
	if err != nil {
		t.Fatalf("ParseBookRef: %v", err)
	}
	if plain.URL != "https://ecampusontario.pressbooks.pub/northern/" {
		t.Errorf("URL = %q, want %q", plain.URL, "https://ecampusontario.pressbooks.pub/northern/")
	}
}

func TestBookRefAPIBaseAndExportURL(t *testing.T) {
	plain, err := ParseBookRef("https://ecampusontario.pressbooks.pub/northern/")
	if err != nil {
		t.Fatalf("ParseBookRef: %v", err)
	}
	if got, want := plain.APIBase(), "https://ecampusontario.pressbooks.pub/northern/wp-json/pressbooks/v2"; got != want {
		t.Errorf("APIBase() = %q, want %q", got, want)
	}
	if got, want := plain.ExportURL("epub"), "https://ecampusontario.pressbooks.pub/northern/open/download?type=epub"; got != want {
		t.Errorf("ExportURL(%q) = %q, want %q", "epub", got, want)
	}

	withPort, err := ParseBookRef("http://127.0.0.1:8080/northern")
	if err != nil {
		t.Fatalf("ParseBookRef: %v", err)
	}
	if got, want := withPort.APIBase(), "http://127.0.0.1:8080/northern/wp-json/pressbooks/v2"; got != want {
		t.Errorf("APIBase() = %q, want %q", got, want)
	}
	if got, want := withPort.ExportURL("pdf"), "http://127.0.0.1:8080/northern/open/download?type=pdf"; got != want {
		t.Errorf("ExportURL(%q) = %q, want %q", "pdf", got, want)
	}
}

func TestParseBookRefRejectsUnusableInput(t *testing.T) {
	for _, in := range []string{"", "   ", "not a url", "https:///nohost"} {
		if _, err := ParseBookRef(in); err == nil {
			t.Errorf("ParseBookRef(%q) succeeded, want an error", in)
		}
	}
}

func TestParsePageRef(t *testing.T) {
	tests := []struct {
		in       string
		wantID   int
		wantSlug string
	}{
		{"5", 5, ""},
		{"chapter-1", 0, "chapter-1"},
		{"chapter/chapter-1", 0, "chapter-1"},
		{"front-matter/introduction", 0, "introduction"},
		{"https://ecampusontario.pressbooks.pub/northern/chapter/chapter-1/", 0, "chapter-1"},
	}
	for _, tt := range tests {
		gotID, gotSlug := ParsePageRef(tt.in)
		if gotID != tt.wantID || gotSlug != tt.wantSlug {
			t.Errorf("ParsePageRef(%q) = (%d, %q), want (%d, %q)", tt.in, gotID, gotSlug, tt.wantID, tt.wantSlug)
		}
	}
}
