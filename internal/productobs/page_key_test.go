package productobs

import "testing"

func TestPageKeyNormalization(t *testing.T) {
	for _, tc := range []struct{ segment, want string }{
		{"Abito-in-cotone-e-seta-251MH126A4608", "abito-in-cotone-e-seta-251mh126a4608"},
		{"MAGLIA-IN-SOgno_251M%9Cart.HTML", "maglia-in-sogno_251m%9cart"},
		{"IMPLANT-KIT.HTM", "implant-kit"},
		{"IMPLANT-KIT.PHP", "implant-kit"},
		{"IMPLANT-KIT.ASP", "implant-kit"},
		{"IMPLANT-KIT.ASPX", "implant-kit"},
		{"IMPLANT-KIT.JSP", "implant-kit"},
		{"implant-kit.html.html", "implant-kit.html"},
		{"%20implant-kit.html", "implant-kit"},
		{"ＦＵＬＬ－ＷＩＤＴＨ．ＨＴＭＬ", "full-width"},
		{"CAFE\u0301-CLINIQUE", "café-clinique"},
		{"Abc-１２３４", "abc-1234"},
		{"ΟΣ-ΚΟΣΜΟΣ", "ος-κοσμος"},
		{"implant%21-kit.html", "implant%21-kit"},
		{"implant%2Fkit-extra.html", ""},
		{"implant%252Fkit-extra.html", ""},
		{"implant%00-kit.html", ""},
		{"implant%0A-kit.html", ""},
		{"implant／kit-extra.html", ""},
		{"implant-kit\u0301\u0301.html", ""},
		{"implant-kit%CC%81%CC%81.html", ""},
		{"İMPLANT-KIT.html", ""},
		{"ab-1234.html", ""},
		{"ab-12345.html", "ab-12345"},
		{"1234-5678.html", ""},
		{"implantkit.html", ""},
		{"", ""},
	} {
		t.Run(tc.segment, func(t *testing.T) {
			check := func(side string, key string, ok bool) {
				t.Helper()
				if key != tc.want || ok != (tc.want != "") {
					t.Errorf("%s key = %q, %t, want %q", side, key, ok, tc.want)
				}
			}
			key, ok := ProductPageKey("https://shop.example/it/" + tc.segment)
			check("product", key, ok)
			key, ok = SourcePageKey("bc-it/catalog/" + tc.segment)
			check("source", key, ok)
		})
	}
}

func TestPageKeyInputShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  func(string) (string, bool)
		in   string
		want string
	}{
		{"product query and fragment", ProductPageKey, "HTTPS://shop.example/it/IMPLANT-KIT.html///?other=ignored-item#fragment-item", "implant-kit"},
		{"product malformed escape", ProductPageKey, "https://x/%ZZ/implant-kit.html", ""},
		{"product malformed final escape", ProductPageKey, "https://x/implant%ZZ-kit.html", ""},
		{"product empty path", ProductPageKey, "https://host/", ""},
		{"product authority only", ProductPageKey, "//host/implant-kit.html", ""},
		{"product file URL", ProductPageKey, "file:///manuals/implant-kit.html", ""},
		{"product opaque HTTP", ProductPageKey, "http:foo/bar", ""},
		{"product no hostname", ProductPageKey, "https://:443/implant-kit.html", ""},
		{"source malformed escape fallback", SourcePageKey, "bc-it/IMPLANT%ZZ-KIT.HTML", "implant%zz-kit"},
		{"source truncated escape fallback", SourcePageKey, "bc-it/IMPLANT-KIT%.HTML", "implant-kit%"},
		{"source bare token", SourcePageKey, "implant-kit.html", ""},
		{"source bare text", SourcePageKey, "this is an implant-kit.html", ""},
		{"source no path", SourcePageKey, "bc-it/", ""},
		{"source empty segment", SourcePageKey, "bc-it//implant-kit.html", ""},
		{"source trailing slash", SourcePageKey, "bc-it/implant-kit.html/", ""},
		{"source whitespace", SourcePageKey, "bc-it/with space/implant-kit.html", ""},
		{"source query", SourcePageKey, "bc-it/implant-kit.html?sku=1", ""},
		{"source fragment", SourcePageKey, "bc-it/implant-kit.html#detail", ""},
		{"source invalid namespace", SourcePageKey, "bè/implant-kit.html", ""},
		{"source absolute path", SourcePageKey, "/bc-it/implant-kit.html", ""},
		{"source absolute URL", SourcePageKey, "https://shop.example/implant-kit.html", ""},
		{"source malformed URL", SourcePageKey, "https://foreign.example/%ZZ/implant-kit.html", ""},
		{"source authority only", SourcePageKey, "//foreign.example/implant-kit.html", ""},
		{"source file URL", SourcePageKey, "file:///manuals/implant-kit.html", ""},
		{"source opaque HTTP", SourcePageKey, "http:foo/bar", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, ok := tc.key(tc.in)
			if key != tc.want || ok != (tc.want != "") {
				t.Errorf("key(%q) = %q, %t, want %q", tc.in, key, ok, tc.want)
			}
		})
	}
}
