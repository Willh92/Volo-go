package tinyxml

import (
	"strings"
	"testing"
)

func TestParse_S3ListBucketResult(t *testing.T) {
	raw := []byte("\xEF\xBB\xBF" + Header + `
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>volo-backups</Name>
  <IsTruncated>true</IsTruncated>
  <NextContinuationToken>tok+2/a&amp;b</NextContinuationToken>
  <Contents>
    <Key>daily/volo-auto-20261109-010000-001.zip</Key>
    <LastModified>2026-11-09T01:00:00Z</LastModified>
    <Size>4096</Size>
  </Contents>
  <Contents>
    <Key>daily/volo-auto-20261109-020000-002.zip</Key>
    <LastModified>2026-11-09T02:00:00.500Z</LastModified>
    <Size>8192</Size>
  </Contents>
</ListBucketResult>`)

	root, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse S3 XML failed: %v", err)
	}
	if root.Local != "ListBucketResult" {
		t.Fatalf("root.Local = %q, want ListBucketResult", root.Local)
	}
	if root.ChildText("IsTruncated") != "true" {
		t.Fatalf("IsTruncated = %q", root.ChildText("IsTruncated"))
	}
	if root.ChildText("NextContinuationToken") != "tok+2/a&b" {
		t.Fatalf("NextContinuationToken = %q, want tok+2/a&b", root.ChildText("NextContinuationToken"))
	}
	items := root.ChildrenByLocal("Contents")
	if len(items) != 2 {
		t.Fatalf("len(Contents) = %d, want 2", len(items))
	}
	if items[0].ChildText("Key") != "daily/volo-auto-20261109-010000-001.zip" || items[1].ChildText("Size") != "8192" {
		t.Fatalf("unexpected Contents values: %+v / %+v", items[0], items[1])
	}
}

func TestParse_WebDAVMultistatusPrefixes(t *testing.T) {
	raw := []byte(`<?xml version="1.0" encoding="utf-8"?>
<!-- WebDAV response with mixed D: and d: namespace prefixes -->
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>/dav/volo-nightly/</D:href>
    <D:propstat>
      <D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop>
    </D:propstat>
  </D:response>
  <d:response xmlns:d="DAV:">
    <d:href>/dav/volo-nightly/volo-auto-20261109-030000-001.zip</d:href>
    <d:propstat>
      <d:prop>
        <d:getlastmodified>Mon, 09 Nov 2026 03:00:00 GMT</d:getlastmodified>
      </d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
</D:multistatus>`)

	root, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse WebDAV XML failed: %v", err)
	}
	if root.Prefix != "D" || root.Local != "multistatus" {
		t.Fatalf("unexpected root %s:%s", root.Prefix, root.Local)
	}
	resps := root.ChildrenByLocal("response")
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(resps))
	}
	r1 := resps[1]
	if got := r1.ChildText("href"); got != "/dav/volo-nightly/volo-auto-20261109-030000-001.zip" {
		t.Fatalf("href = %q", got)
	}
	ps := r1.Child("propstat")
	if ps == nil || ps.Child("prop") == nil {
		t.Fatal("missing propstat/prop")
	}
	if lm := ps.Child("prop").ChildText("getlastmodified"); lm != "Mon, 09 Nov 2026 03:00:00 GMT" {
		t.Fatalf("getlastmodified = %q", lm)
	}
}

func TestParse_RSS2WithDCCreatorCDATAAndDoctype(t *testing.T) {
	raw := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE rss [
  <!ELEMENT rss ANY>
  <!-- internal subset comment with > inside -->
  <!ENTITY note "ignored>here">
]>
<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:content="http://purl.org/rss/1.0/modules/content/">
  <channel>
    <title>Volo &amp; Friends &#x1F680;</title>
    <item>
      <title>Hello &#65;BC</title>
      <dc:creator>Will &apos;Author&apos;</dc:creator>
      <description><![CDATA[<p>Raw <b>HTML</b> & unescaped <tags>!</p>]]></description>
      <content:encoded><![CDATA[Full article body]]></content:encoded>
    </item>
  </channel>
</rss>`)

	root, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse RSS failed: %v", err)
	}
	if root.AttrExact("version") != "2.0" {
		t.Fatalf("version attr = %q", root.AttrExact("version"))
	}
	ch := root.Child("channel")
	if ch == nil {
		t.Fatal("missing channel")
	}
	if got := ch.ChildText("title"); got != "Volo & Friends 🚀" {
		t.Fatalf("channel title = %q", got)
	}
	item := ch.Child("item")
	if item == nil {
		t.Fatal("missing item")
	}
	if got := item.ChildText("title"); got != "Hello ABC" {
		t.Fatalf("item title = %q", got)
	}
	if got := item.ChildText("creator"); got != "Will 'Author'" {
		t.Fatalf("creator by local = %q", got)
	}
	if got := item.ChildText("dc:creator"); got != "Will 'Author'" {
		t.Fatalf("dc:creator = %q", got)
	}
	if got := item.ChildText("description"); got != "<p>Raw <b>HTML</b> & unescaped <tags>!</p>" {
		t.Fatalf("CDATA description = %q", got)
	}
	if got := item.ChildText("content:encoded"); got != "Full article body" {
		t.Fatalf("content:encoded = %q", got)
	}
}

func TestParse_AtomLinkAndNestedOPML(t *testing.T) {
	atomXML := []byte(`<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Atom Feed</title>
  <link href='https://blog.example.com/atom.xml' rel='self'/>
  <link href="https://blog.example.com/" rel="alternate"/>
  <entry>
    <title>Post 1</title>
    <link href="https://blog.example.com/posts/1?a=1&amp;b=2" rel="alternate"/>
  </entry>
</feed>`)

	feed, err := Parse(atomXML)
	if err != nil {
		t.Fatalf("Parse Atom failed: %v", err)
	}
	links := feed.ChildrenByLocal("link")
	if len(links) != 2 || links[0].Attr("rel") != "self" || links[1].Attr("HREF") != "https://blog.example.com/" {
		t.Fatalf("unexpected feed links: %+v", links)
	}
	entryLink := feed.Child("entry").Child("link")
	if entryLink == nil || entryLink.AttrExact("href") != "https://blog.example.com/posts/1?a=1&b=2" {
		t.Fatalf("unexpected entry link: %+v", entryLink)
	}

	opmlXML := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>Subscriptions</title></head>
  <body>
    <outline text="Tech" title="Tech">
      <outline type="rss" text="Go Blog" xmlUrl="https://go.dev/blog/feed.atom" htmlUrl="https://go.dev/blog"/>
      <outline type="rss" text="Volo" xmlUrl="https://volo.example/rss.xml"/>
    </outline>
  </body>
</opml>`)

	opml, err := Parse(opmlXML)
	if err != nil {
		t.Fatalf("Parse OPML failed: %v", err)
	}
	cat := opml.Child("body").Child("outline")
	if cat == nil || len(cat.ChildrenByLocal("outline")) != 2 {
		t.Fatalf("expected 2 nested outlines, got %+v", cat)
	}
	first := cat.ChildrenByLocal("outline")[0]
	if first.Attr("xmlurl") != "https://go.dev/blog/feed.atom" || first.AttrExact("xmlUrl") != "https://go.dev/blog/feed.atom" {
		t.Fatalf("unexpected xmlUrl attr: %+v", first.Attrs)
	}
}

func TestParse_MalformedAndLimitsAndEscapeText(t *testing.T) {
	badInputs := []string{
		"",
		"   <!-- only comment -->  ",
		"<root><unclosed></root>",
		"<root>",
		"</orphan>",
		"<root attr=unquoted/>",
		"<root attr=\"unclosed/>",
		"<root attr=\"bad<val\"/>",
		"<root>&unknownEntity;</root>",
		"<root>&#xD800;</root>",
		"<root><![CDATA[unterminated</root>",
		"<!-- unclosed comment",
		"<?xml unclosed pi",
		"<!DOCTYPE root [ <!ELEMENT a ANY>",
		"<a/><b/>",
		"stray<a/>",
	}
	for _, in := range badInputs {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("expected Parse(%q) to fail, but succeeded", in)
		}
	}

	// Exceed maxNestingDepth (64)
	var deep strings.Builder
	for i := 0; i < 65; i++ {
		deep.WriteString("<n>")
	}
	for i := 0; i < 65; i++ {
		deep.WriteString("</n>")
	}
	if _, err := Parse([]byte(deep.String())); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("expected max nesting depth error, got %v", err)
	}

	// EscapeText strips XML 1.0 illegal control bytes while preserving \t, \n, \r and escaping entities
	escaped := EscapeText("A&B <C> \"D\" 'E'\x00\x07\x1f\t\n\r完")
	want := "A&amp;B &lt;C&gt; &quot;D&quot; &apos;E&apos;\t\n\r完"
	if escaped != want {
		t.Fatalf("EscapeText = %q, want %q", escaped, want)
	}
}
