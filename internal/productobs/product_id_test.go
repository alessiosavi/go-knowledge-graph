package productobs

import "testing"

func TestProductID(t *testing.T) {
	const store = "example.test"
	for _, tc := range []struct{ productID, key, digest string }{
		{"example.test:1", `["product","example.test","example.test:1"]`, "6844cb2d7e1875c2e5efb99e953afdac"},
		{"example.test:a&b<é>", `["product","example.test","example.test:a&b<é>"]`, "4d8e05cbd600728457b288fffe20b27b"},
	} {
		t.Run(tc.productID, func(t *testing.T) {
			if got := string(productIDBytes(store, tc.productID)); got != tc.key {
				t.Errorf("product ID bytes = %q, want %q", got, tc.key)
			}
			if got := ProductID(store, tc.productID); got != tc.digest {
				t.Errorf("product ID = %q, want %q", got, tc.digest)
			}
		})
	}
}
