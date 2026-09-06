package pressbooks

import "time"

// Network is one Pressbooks network from the bundled list.
type Network struct {
	Host  string `json:"host"`
	Name  string `json:"name"`
	Books int    `json:"books"`
	// Note records why this entry differs from what the host self-reports.
	Note string `json:"note,omitempty"`
}

// ExcludedNetwork is a host that was probed and found unusable. It is recorded
// so that a host nobody can reach is not re-added by someone who rediscovers
// the problem by hand.
type ExcludedNetwork struct {
	Host   string `json:"host"`
	Reason string `json:"reason"`
}

type NetworkList struct {
	Networks []Network         `json:"networks"`
	Excluded []ExcludedNetwork `json:"excluded"`
}

// Book is one book in a network catalog.
type Book struct {
	URL           string   `json:"url"`
	Host          string   `json:"host"`
	Title         string   `json:"title"`
	Subtitle      string   `json:"subtitle,omitempty"`
	Authors       []string `json:"authors,omitempty"`
	LicenseName   string   `json:"license_name,omitempty"`
	LicenseURL    string   `json:"license_url,omitempty"`
	Language      string   `json:"language,omitempty"`
	CopyrightYear string   `json:"copyright_year,omitempty"`
	CoverURL      string   `json:"cover_url,omitempty"`
	WordCount     int      `json:"word_count"`
	NetworkName   string   `json:"network_name,omitempty"`
}

// Catalog is one network's books, as cached on disk.
type Catalog struct {
	Host        string    `json:"host"`
	Name        string    `json:"name"`
	Books       []Book    `json:"books"`
	TotalBooks  int       `json:"total_books"`
	SyncedPages int       `json:"synced_pages"`
	TotalPages  int       `json:"total_pages"`
	SyncedAt    time.Time `json:"synced_at"`
}

type TOC struct {
	FrontMatter []TOCItem `json:"front-matter"`
	Parts       []TOCPart `json:"parts"`
	BackMatter  []TOCItem `json:"back-matter"`
}

type TOCPart struct {
	ID             int       `json:"id"`
	Title          string    `json:"title"`
	Slug           string    `json:"slug"`
	Status         string    `json:"status"`
	MenuOrder      int       `json:"menu_order"`
	HasPostContent bool      `json:"has_post_content"`
	WordCount      int       `json:"word_count"`
	Link           string    `json:"link"`
	Chapters       []TOCItem `json:"chapters"`
}

type TOCItem struct {
	ID             int    `json:"id"`
	Title          string `json:"title"`
	Slug           string `json:"slug"`
	Status         string `json:"status"`
	MenuOrder      int    `json:"menu_order"`
	Export         bool   `json:"export"`
	HasPostContent bool   `json:"has_post_content"`
	WordCount      int    `json:"word_count"`
	Link           string `json:"link"`
}

// Page is one extractable page, flattened out of a TOC in reading order.
//
// Type is the REST collection the content lives in: "front-matter",
// "chapters", "back-matter", or "parts".
type Page struct {
	ID        int    `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Slug      string `json:"slug"`
	URL       string `json:"url"`
	PartTitle string `json:"part_title,omitempty"`
	WordCount int    `json:"word_count"`
}

type Image struct {
	Src string `json:"src"`
	Alt string `json:"alt,omitempty"`
}

type ExtractedPage struct {
	BookURL   string  `json:"book_url"`
	BookTitle string  `json:"book_title"`
	PageID    int     `json:"page_id"`
	PageType  string  `json:"page_type"`
	PageTitle string  `json:"page_title"`
	PageSlug  string  `json:"page_slug"`
	SourceURL string  `json:"source_url"`
	Text      string  `json:"text"`
	HTML      string  `json:"html,omitempty"`
	Images    []Image `json:"images,omitempty"`
}

// flexString decodes a JSON field that some hosts send as a string and others
// as a number. copyrightYear is the field that forced this: it arrives as
// "2017" on one network and 2017 on another, and a plain string field makes the
// whole book fail to decode on the second.
type flexString string
